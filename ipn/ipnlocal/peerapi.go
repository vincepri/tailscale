// Copyright (c) 2021 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ipnlocal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"html"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kortschak/wol"
	"golang.org/x/net/dns/dnsmessage"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/health"
	"tailscale.com/hostinfo"
	"tailscale.com/ipn"
	"tailscale.com/logtail/backoff"
	"tailscale.com/net/dns/resolver"
	"tailscale.com/net/interfaces"
	"tailscale.com/net/netaddr"
	"tailscale.com/net/netutil"
	"tailscale.com/tailcfg"
	"tailscale.com/util/clientmetric"
	"tailscale.com/util/strs"
	"tailscale.com/wgengine"
	"tailscale.com/wgengine/filter"
)

var initListenConfig func(*net.ListenConfig, netip.Addr, *interfaces.State, string) error

// addH2C is non-nil on platforms where we want to add H2C
// ("cleartext" HTTP/2) support to the peerAPI.
var addH2C func(*http.Server)

type peerAPIServer struct {
	b          *LocalBackend
	rootDir    string // empty means file receiving unavailable
	selfNode   *tailcfg.Node
	knownEmpty atomic.Bool
	resolver   *resolver.Resolver

	// directFileMode is whether we're writing files directly to a
	// download directory (as *.partial files), rather than making
	// the frontend retrieve it over localapi HTTP and write it
	// somewhere itself. This is used on the GUI macOS versions
	// and on Synology.
	// In directFileMode, the peerapi doesn't do the final rename
	// from "foo.jpg.partial" to "foo.jpg" unless
	// directFileDoFinalRename is set.
	directFileMode bool

	// directFileDoFinalRename is whether in directFileMode we
	// additionally move the *.direct file to its final name after
	// it's received.
	directFileDoFinalRename bool
}

const (
	// partialSuffix is the suffix appened to files while they're
	// still in the process of being transferred.
	partialSuffix = ".partial"

	// deletedSuffix is the suffix for a deleted marker file
	// that's placed next to a file (without the suffix) that we
	// tried to delete, but Windows wouldn't let us. These are
	// only written on Windows (and in tests), but they're not
	// permitted to be uploaded directly on any platform, like
	// partial files.
	deletedSuffix = ".deleted"
)

func (s *peerAPIServer) canReceiveFiles() bool {
	return s != nil && s.rootDir != ""
}

func validFilenameRune(r rune) bool {
	switch r {
	case '/':
		return false
	case '\\', ':', '*', '"', '<', '>', '|':
		// Invalid stuff on Windows, but we reject them everywhere
		// for now.
		// TODO(bradfitz): figure out a better plan. We initially just
		// wrote things to disk URL path-escaped, but that's gross
		// when debugging, and just moves the problem to callers.
		// So now we put the UTF-8 filenames on disk directly as
		// sent.
		return false
	}
	return unicode.IsPrint(r)
}

func (s *peerAPIServer) diskPath(baseName string) (fullPath string, ok bool) {
	if !utf8.ValidString(baseName) {
		return "", false
	}
	if strings.TrimSpace(baseName) != baseName {
		return "", false
	}
	if len(baseName) > 255 {
		return "", false
	}
	// TODO: validate unicode normalization form too? Varies by platform.
	clean := path.Clean(baseName)
	if clean != baseName ||
		clean == "." || clean == ".." ||
		strings.HasSuffix(clean, deletedSuffix) ||
		strings.HasSuffix(clean, partialSuffix) {
		return "", false
	}
	for _, r := range baseName {
		if !validFilenameRune(r) {
			return "", false
		}
	}
	return filepath.Join(s.rootDir, baseName), true
}

// hasFilesWaiting reports whether any files are buffered in the
// tailscaled daemon storage.
func (s *peerAPIServer) hasFilesWaiting() bool {
	if s == nil || s.rootDir == "" || s.directFileMode {
		return false
	}
	if s.knownEmpty.Load() {
		// Optimization: this is usually empty, so avoid opening
		// the directory and checking. We can't cache the actual
		// has-files-or-not values as the macOS/iOS client might
		// in the future use+delete the files directly. So only
		// keep this negative cache.
		return false
	}
	f, err := os.Open(s.rootDir)
	if err != nil {
		return false
	}
	defer f.Close()
	for {
		des, err := f.ReadDir(10)
		for _, de := range des {
			name := de.Name()
			if strings.HasSuffix(name, partialSuffix) {
				continue
			}
			if strings.HasSuffix(name, deletedSuffix) { // for Windows + tests
				// After we're done looping over files, then try
				// to delete this file. Don't do it proactively,
				// as the OS may return "foo.jpg.deleted" before "foo.jpg"
				// and we don't want to delete the ".deleted" file before
				// enumerating to the "foo.jpg" file.
				defer tryDeleteAgain(filepath.Join(s.rootDir, strings.TrimSuffix(name, deletedSuffix)))
				continue
			}
			if de.Type().IsRegular() {
				_, err := os.Stat(filepath.Join(s.rootDir, name+deletedSuffix))
				if os.IsNotExist(err) {
					return true
				}
				if err == nil {
					tryDeleteAgain(filepath.Join(s.rootDir, name))
					continue
				}
			}
		}
		if err == io.EOF {
			s.knownEmpty.Store(true)
		}
		if err != nil {
			break
		}
	}
	return false
}

// WaitingFiles returns the list of files that have been sent by a
// peer that are waiting in the buffered "pick up" directory owned by
// the Tailscale daemon.
//
// As a side effect, it also does any lazy deletion of files as
// required by Windows.
func (s *peerAPIServer) WaitingFiles() (ret []apitype.WaitingFile, err error) {
	if s == nil {
		return nil, errNilPeerAPIServer
	}
	if s.rootDir == "" {
		return nil, errNoTaildrop
	}
	if s.directFileMode {
		return nil, nil
	}
	f, err := os.Open(s.rootDir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var deleted map[string]bool // "foo.jpg" => true (if "foo.jpg.deleted" exists)
	for {
		des, err := f.ReadDir(10)
		for _, de := range des {
			name := de.Name()
			if strings.HasSuffix(name, partialSuffix) {
				continue
			}
			if strings.HasSuffix(name, deletedSuffix) { // for Windows + tests
				if deleted == nil {
					deleted = map[string]bool{}
				}
				deleted[strings.TrimSuffix(name, deletedSuffix)] = true
				continue
			}
			if de.Type().IsRegular() {
				fi, err := de.Info()
				if err != nil {
					continue
				}
				ret = append(ret, apitype.WaitingFile{
					Name: filepath.Base(name),
					Size: fi.Size(),
				})
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if len(deleted) > 0 {
		// Filter out any return values "foo.jpg" where a
		// "foo.jpg.deleted" marker file exists on disk.
		all := ret
		ret = ret[:0]
		for _, wf := range all {
			if !deleted[wf.Name] {
				ret = append(ret, wf)
			}
		}
		// And do some opportunistic deleting while we're here.
		// Maybe Windows is done virus scanning the file we tried
		// to delete a long time ago and will let us delete it now.
		for name := range deleted {
			tryDeleteAgain(filepath.Join(s.rootDir, name))
		}
	}
	sort.Slice(ret, func(i, j int) bool { return ret[i].Name < ret[j].Name })
	return ret, nil
}

var (
	errNilPeerAPIServer = errors.New("peerapi unavailable; not listening")
	errNoTaildrop       = errors.New("Taildrop disabled; no storage directory")
)

// tryDeleteAgain tries to delete path (and path+deletedSuffix) after
// it failed earlier.  This happens on Windows when various anti-virus
// tools hook into filesystem operations and have the file open still
// while we're trying to delete it. In that case we instead mark it as
// deleted (writing a "foo.jpg.deleted" marker file), but then we
// later try to clean them up.
//
// fullPath is the full path to the file without the deleted suffix.
func tryDeleteAgain(fullPath string) {
	if err := os.Remove(fullPath); err == nil || os.IsNotExist(err) {
		os.Remove(fullPath + deletedSuffix)
	}
}

func (s *peerAPIServer) DeleteFile(baseName string) error {
	if s == nil {
		return errNilPeerAPIServer
	}
	if s.rootDir == "" {
		return errNoTaildrop
	}
	if s.directFileMode {
		return errors.New("deletes not allowed in direct mode")
	}
	path, ok := s.diskPath(baseName)
	if !ok {
		return errors.New("bad filename")
	}
	var bo *backoff.Backoff
	logf := s.b.logf
	t0 := time.Now()
	for {
		err := os.Remove(path)
		if err != nil && !os.IsNotExist(err) {
			err = redactErr(err)
			// Put a retry loop around deletes on Windows. Windows
			// file descriptor closes are effectively asynchronous,
			// as a bunch of hooks run on/after close, and we can't
			// necessarily delete the file for a while after close,
			// as we need to wait for everybody to be done with
			// it. (on Windows, unlike Unix, a file can't be deleted
			// if it's open anywhere)
			// So try a few times but ultimately just leave a
			// "foo.jpg.deleted" marker file to note that it's
			// deleted and we clean it up later.
			if runtime.GOOS == "windows" {
				if bo == nil {
					bo = backoff.NewBackoff("delete-retry", logf, 1*time.Second)
				}
				if time.Since(t0) < 5*time.Second {
					bo.BackOff(context.Background(), err)
					continue
				}
				if err := touchFile(path + deletedSuffix); err != nil {
					logf("peerapi: failed to leave deleted marker: %v", err)
				}
			}
			logf("peerapi: failed to DeleteFile: %v", err)
			return err
		}
		return nil
	}
}

// redacted is a fake path name we use in errors, to avoid
// accidentally logging actual filenames anywhere.
const redacted = "redacted"

func redactErr(err error) error {
	if pe, ok := err.(*os.PathError); ok {
		pe.Path = redacted
	}
	return err
}

func touchFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		return redactErr(err)
	}
	return f.Close()
}

func (s *peerAPIServer) OpenFile(baseName string) (rc io.ReadCloser, size int64, err error) {
	if s == nil {
		return nil, 0, errNilPeerAPIServer
	}
	if s.rootDir == "" {
		return nil, 0, errNoTaildrop
	}
	if s.directFileMode {
		return nil, 0, errors.New("opens not allowed in direct mode")
	}
	path, ok := s.diskPath(baseName)
	if !ok {
		return nil, 0, errors.New("bad filename")
	}
	if fi, err := os.Stat(path + deletedSuffix); err == nil && fi.Mode().IsRegular() {
		tryDeleteAgain(path)
		return nil, 0, &fs.PathError{Op: "open", Path: redacted, Err: fs.ErrNotExist}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, redactErr(err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, redactErr(err)
	}
	return f, fi.Size(), nil
}

func (s *peerAPIServer) listen(ip netip.Addr, ifState *interfaces.State) (ln net.Listener, err error) {
	// Android for whatever reason often has problems creating the peerapi listener.
	// But since we started intercepting it with netstack, it's not even important that
	// we have a real kernel-level listener. So just create a dummy listener on Android
	// and let netstack intercept it.
	if runtime.GOOS == "android" {
		return newFakePeerAPIListener(ip), nil
	}

	ipStr := ip.String()

	var lc net.ListenConfig
	if initListenConfig != nil {
		// On iOS/macOS, this sets the lc.Control hook to
		// setsockopt the interface index to bind to, to get
		// out of the network sandbox.
		if err := initListenConfig(&lc, ip, ifState, s.b.dialer.TUNName()); err != nil {
			return nil, err
		}
		if runtime.GOOS == "darwin" || runtime.GOOS == "ios" {
			ipStr = ""
		}
	}

	if wgengine.IsNetstack(s.b.e) {
		ipStr = ""
	}

	tcp4or6 := "tcp4"
	if ip.Is6() {
		tcp4or6 = "tcp6"
	}

	// Make a best effort to pick a deterministic port number for
	// the ip. The lower three bytes are the same for IPv4 and IPv6
	// Tailscale addresses (at least currently), so we'll usually
	// get the same port number on both address families for
	// dev/debugging purposes, which is nice. But it's not so
	// deterministic that people will bake this into clients.
	// We try a few times just in case something's already
	// listening on that port (on all interfaces, probably).
	for try := uint8(0); try < 5; try++ {
		a16 := ip.As16()
		hashData := a16[len(a16)-3:]
		hashData[0] += try
		tryPort := (32 << 10) | uint16(crc32.ChecksumIEEE(hashData))
		ln, err = lc.Listen(context.Background(), tcp4or6, net.JoinHostPort(ipStr, strconv.Itoa(int(tryPort))))
		if err == nil {
			return ln, nil
		}
	}
	// Fall back to some random ephemeral port.
	ln, err = lc.Listen(context.Background(), tcp4or6, net.JoinHostPort(ipStr, "0"))

	// And if we're on a platform with netstack (anything but iOS), then just fallback to netstack.
	if err != nil && runtime.GOOS != "ios" {
		s.b.logf("peerapi: failed to do peerAPI listen, harmless (netstack available) but error was: %v", err)
		return newFakePeerAPIListener(ip), nil
	}
	return ln, err
}

type peerAPIListener struct {
	ps *peerAPIServer
	ip netip.Addr
	lb *LocalBackend

	// ln is the Listener. It can be nil in netstack mode if there are more than
	// 1 local addresses (e.g. both an IPv4 and IPv6). When it's nil, port
	// and urlStr are still populated.
	ln net.Listener

	// urlStr is the base URL to access the peer API (http://ip:port/).
	urlStr string
	// port is just the port of urlStr.
	port int
}

func (pln *peerAPIListener) Close() error {
	if pln.ln != nil {
		return pln.ln.Close()
	}
	return nil
}

func (pln *peerAPIListener) serve() {
	if pln.ln == nil {
		return
	}
	defer pln.ln.Close()
	logf := pln.lb.logf
	for {
		c, err := pln.ln.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			logf("peerapi.Accept: %v", err)
			return
		}
		ta, ok := c.RemoteAddr().(*net.TCPAddr)
		if !ok {
			c.Close()
			logf("peerapi: unexpected RemoteAddr %#v", c.RemoteAddr())
			continue
		}
		ipp := netaddr.Unmap(ta.AddrPort())
		if !ipp.IsValid() {
			logf("peerapi: bogus TCPAddr %#v", ta)
			c.Close()
			continue
		}
		pln.ServeConn(ipp, c)
	}
}

func (pln *peerAPIListener) ServeConn(src netip.AddrPort, c net.Conn) {
	logf := pln.lb.logf
	peerNode, peerUser, ok := pln.lb.WhoIs(src)
	if !ok {
		logf("peerapi: unknown peer %v", src)
		c.Close()
		return
	}
	h := &peerAPIHandler{
		ps:         pln.ps,
		isSelf:     pln.ps.selfNode.User == peerNode.User,
		remoteAddr: src,
		peerNode:   peerNode,
		peerUser:   peerUser,
	}
	httpServer := &http.Server{
		Handler: h,
	}
	if addH2C != nil {
		addH2C(httpServer)
	}
	go httpServer.Serve(netutil.NewOneConnListener(c, pln.ln.Addr()))
}

// peerAPIHandler serves the Peer API for a source specific client.
type peerAPIHandler struct {
	ps         *peerAPIServer
	remoteAddr netip.AddrPort
	isSelf     bool                // whether peerNode is owned by same user as this node
	peerNode   *tailcfg.Node       // peerNode is who's making the request
	peerUser   tailcfg.UserProfile // profile of peerNode
}

func (h *peerAPIHandler) logf(format string, a ...any) {
	h.ps.b.logf("peerapi: "+format, a...)
}

func (h *peerAPIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v0/put/") {
		h.handlePeerPut(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/dns-query") {
		h.handleDNSQuery(w, r)
		return
	}
	switch r.URL.Path {
	case "/v0/goroutines":
		h.handleServeGoroutines(w, r)
		return
	case "/v0/env":
		h.handleServeEnv(w, r)
		return
	case "/v0/metrics":
		h.handleServeMetrics(w, r)
		return
	case "/v0/magicsock":
		h.handleServeMagicsock(w, r)
		return
	case "/v0/dnsfwd":
		h.handleServeDNSFwd(w, r)
		return
	case "/v0/wol":
		h.handleWakeOnLAN(w, r)
		return
	case "/v0/interfaces":
		h.handleServeInterfaces(w, r)
		return
	}
	who := h.peerUser.DisplayName
	fmt.Fprintf(w, `<html>
<meta name="viewport" content="width=device-width, initial-scale=1">
<body>
<h1>Hello, %s (%v)</h1>
This is my Tailscale device. Your device is %v.
`, html.EscapeString(who), h.remoteAddr.Addr(), html.EscapeString(h.peerNode.ComputedName))

	if h.isSelf {
		fmt.Fprintf(w, "<p>You are the owner of this node.\n")
	}
}

func (h *peerAPIHandler) handleServeInterfaces(w http.ResponseWriter, r *http.Request) {
	if !h.canDebug() {
		http.Error(w, "denied; no debug access", http.StatusForbidden)
		return
	}
	i, err := interfaces.GetList()
	if err != nil {
		http.Error(w, err.Error(), 500)
	}

	dr, err := interfaces.DefaultRoute()
	if err != nil {
		http.Error(w, err.Error(), 500)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintln(w, "<h1>Interfaces</h1>")
	fmt.Fprintf(w, "<h3>Default route is %q(%d)</h3>\n", dr.InterfaceName, dr.InterfaceIndex)

	fmt.Fprintln(w, "<table>")
	fmt.Fprint(w, "<tr>")
	for _, v := range []any{"Index", "Name", "MTU", "Flags", "Addrs"} {
		fmt.Fprintf(w, "<th>%v</th> ", v)
	}
	fmt.Fprint(w, "</tr>\n")
	i.ForeachInterface(func(iface interfaces.Interface, ipps []netip.Prefix) {
		fmt.Fprint(w, "<tr>")
		for _, v := range []any{iface.Index, iface.Name, iface.MTU, iface.Flags, ipps} {
			fmt.Fprintf(w, "<td>%v</td> ", v)
		}
		fmt.Fprint(w, "</tr>\n")
	})
	fmt.Fprintln(w, "</table>")
}

type incomingFile struct {
	name        string // "foo.jpg"
	started     time.Time
	size        int64     // or -1 if unknown; never 0
	w           io.Writer // underlying writer
	ph          *peerAPIHandler
	partialPath string // non-empty in direct mode

	mu         sync.Mutex
	copied     int64
	done       bool
	lastNotify time.Time
}

func (f *incomingFile) markAndNotifyDone() {
	f.mu.Lock()
	f.done = true
	f.mu.Unlock()
	b := f.ph.ps.b
	b.sendFileNotify()
}

func (f *incomingFile) Write(p []byte) (n int, err error) {
	n, err = f.w.Write(p)

	b := f.ph.ps.b
	var needNotify bool
	defer func() {
		if needNotify {
			b.sendFileNotify()
		}
	}()
	if n > 0 {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.copied += int64(n)
		now := time.Now()
		if f.lastNotify.IsZero() || now.Sub(f.lastNotify) > time.Second {
			f.lastNotify = now
			needNotify = true
		}
	}
	return n, err
}

func (f *incomingFile) PartialFile() ipn.PartialFile {
	f.mu.Lock()
	defer f.mu.Unlock()
	return ipn.PartialFile{
		Name:         f.name,
		Started:      f.started,
		DeclaredSize: f.size,
		Received:     f.copied,
		PartialPath:  f.partialPath,
		Done:         f.done,
	}
}

// canPutFile reports whether h can put a file ("Taildrop") to this node.
func (h *peerAPIHandler) canPutFile() bool {
	return h.isSelf || h.peerHasCap(tailcfg.CapabilityFileSharingSend)
}

// canDebug reports whether h can debug this node (goroutines, metrics,
// magicsock internal state, etc).
func (h *peerAPIHandler) canDebug() bool {
	return h.isSelf || h.peerHasCap(tailcfg.CapabilityDebugPeer)
}

// canWakeOnLAN reports whether h can send a Wake-on-LAN packet from this node.
func (h *peerAPIHandler) canWakeOnLAN() bool {
	return h.isSelf || h.peerHasCap(tailcfg.CapabilityWakeOnLAN)
}

func (h *peerAPIHandler) peerHasCap(wantCap string) bool {
	for _, hasCap := range h.ps.b.PeerCaps(h.remoteAddr.Addr()) {
		if hasCap == wantCap {
			return true
		}
	}
	return false
}

func (h *peerAPIHandler) handlePeerPut(w http.ResponseWriter, r *http.Request) {
	if !h.canPutFile() {
		http.Error(w, "Taildrop access denied", http.StatusForbidden)
		return
	}
	if !h.ps.b.hasCapFileSharing() {
		http.Error(w, "file sharing not enabled by Tailscale admin", http.StatusForbidden)
		return
	}
	if r.Method != "PUT" {
		http.Error(w, "expected method PUT", http.StatusMethodNotAllowed)
		return
	}
	if h.ps.rootDir == "" {
		http.Error(w, errNoTaildrop.Error(), http.StatusInternalServerError)
		return
	}
	rawPath := r.URL.EscapedPath()
	suffix, ok := strs.CutPrefix(rawPath, "/v0/put/")
	if !ok {
		http.Error(w, "misconfigured internals", 500)
		return
	}
	if suffix == "" {
		http.Error(w, "empty filename", 400)
		return
	}
	if strings.Contains(suffix, "/") {
		http.Error(w, "directories not supported", 400)
		return
	}
	baseName, err := url.PathUnescape(suffix)
	if err != nil {
		http.Error(w, "bad path encoding", 400)
		return
	}
	dstFile, ok := h.ps.diskPath(baseName)
	if !ok {
		http.Error(w, "bad filename", 400)
		return
	}
	t0 := time.Now()
	// TODO(bradfitz): prevent same filename being sent by two peers at once
	partialFile := dstFile + partialSuffix
	f, err := os.Create(partialFile)
	if err != nil {
		h.logf("put Create error: %v", redactErr(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var success bool
	defer func() {
		if !success {
			os.Remove(partialFile)
		}
	}()
	var finalSize int64
	var inFile *incomingFile
	if r.ContentLength != 0 {
		inFile = &incomingFile{
			name:    baseName,
			started: time.Now(),
			size:    r.ContentLength,
			w:       f,
			ph:      h,
		}
		if h.ps.directFileMode {
			inFile.partialPath = partialFile
		}
		h.ps.b.registerIncomingFile(inFile, true)
		defer h.ps.b.registerIncomingFile(inFile, false)
		n, err := io.Copy(inFile, r.Body)
		if err != nil {
			err = redactErr(err)
			f.Close()
			h.logf("put Copy error: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		finalSize = n
	}
	if err := redactErr(f.Close()); err != nil {
		h.logf("put Close error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if h.ps.directFileMode && !h.ps.directFileDoFinalRename {
		if inFile != nil { // non-zero length; TODO: notify even for zero length
			inFile.markAndNotifyDone()
		}
	} else {
		if err := os.Rename(partialFile, dstFile); err != nil {
			err = redactErr(err)
			h.logf("put final rename: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	d := time.Since(t0).Round(time.Second / 10)
	h.logf("got put of %s in %v from %v/%v", approxSize(finalSize), d, h.remoteAddr.Addr(), h.peerNode.ComputedName)

	// TODO: set modtime
	// TODO: some real response
	success = true
	io.WriteString(w, "{}\n")
	h.ps.knownEmpty.Store(false)
	h.ps.b.sendFileNotify()
}

func approxSize(n int64) string {
	if n <= 1<<10 {
		return "<=1KB"
	}
	if n <= 1<<20 {
		return "<=1MB"
	}
	return fmt.Sprintf("~%dMB", n>>20)
}

func (h *peerAPIHandler) handleServeGoroutines(w http.ResponseWriter, r *http.Request) {
	if !h.canDebug() {
		http.Error(w, "denied; no debug access", http.StatusForbidden)
		return
	}
	var buf []byte
	for size := 4 << 10; size <= 2<<20; size *= 2 {
		buf = make([]byte, size)
		buf = buf[:runtime.Stack(buf, true)]
		if len(buf) < size {
			break
		}
	}
	w.Write(buf)
}

func (h *peerAPIHandler) handleServeEnv(w http.ResponseWriter, r *http.Request) {
	if !h.canDebug() {
		http.Error(w, "denied; no debug access", http.StatusForbidden)
		return
	}
	var data struct {
		Hostinfo *tailcfg.Hostinfo
		Uid      int
		Args     []string
		Env      []string
	}
	data.Hostinfo = hostinfo.New()
	data.Uid = os.Getuid()
	data.Args = os.Args
	data.Env = os.Environ()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func (h *peerAPIHandler) handleServeMagicsock(w http.ResponseWriter, r *http.Request) {
	if !h.canDebug() {
		http.Error(w, "denied; no debug access", http.StatusForbidden)
		return
	}
	eng := h.ps.b.e
	if ig, ok := eng.(wgengine.InternalsGetter); ok {
		if _, mc, _, ok := ig.GetInternals(); ok {
			mc.ServeHTTPDebug(w, r)
			return
		}
	}
	http.Error(w, "miswired", 500)
}

func (h *peerAPIHandler) handleServeMetrics(w http.ResponseWriter, r *http.Request) {
	if !h.canDebug() {
		http.Error(w, "denied; no debug access", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	clientmetric.WritePrometheusExpositionFormat(w)
}

func (h *peerAPIHandler) handleServeDNSFwd(w http.ResponseWriter, r *http.Request) {
	if !h.canDebug() {
		http.Error(w, "denied; no debug access", http.StatusForbidden)
		return
	}
	dh := health.DebugHandler("dnsfwd")
	if dh == nil {
		http.Error(w, "not wired up", 500)
		return
	}
	dh.ServeHTTP(w, r)
}

func (h *peerAPIHandler) handleWakeOnLAN(w http.ResponseWriter, r *http.Request) {
	if !h.canWakeOnLAN() {
		http.Error(w, "no WoL access", http.StatusForbidden)
		return
	}
	if r.Method != "POST" {
		http.Error(w, "bad method", http.StatusMethodNotAllowed)
		return
	}
	macStr := r.FormValue("mac")
	if macStr == "" {
		http.Error(w, "missing 'mac' param", http.StatusBadRequest)
		return
	}
	mac, err := net.ParseMAC(macStr)
	if err != nil {
		http.Error(w, "bad 'mac' param", http.StatusBadRequest)
		return
	}
	var password []byte // TODO(bradfitz): support?
	st, err := interfaces.GetState()
	if err != nil {
		http.Error(w, "failed to get interfaces state", http.StatusInternalServerError)
		return
	}
	var res struct {
		SentTo []string
		Errors []string
	}
	for ifName, ips := range st.InterfaceIPs {
		for _, ip := range ips {
			if ip.Addr().IsLoopback() || ip.Addr().Is6() {
				continue
			}
			local := &net.UDPAddr{
				IP:   ip.Addr().AsSlice(),
				Port: 0,
			}
			remote := &net.UDPAddr{
				IP:   net.IPv4bcast,
				Port: 0,
			}
			if err := wol.Wake(mac, password, local, remote); err != nil {
				res.Errors = append(res.Errors, err.Error())
			} else {
				res.SentTo = append(res.SentTo, ifName)
			}
			break // one per interface is enough
		}
	}
	sort.Strings(res.SentTo)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func (h *peerAPIHandler) replyToDNSQueries() bool {
	if h.isSelf {
		// If the peer is owned by the same user, just allow it
		// without further checks.
		return true
	}
	b := h.ps.b
	if !b.OfferingExitNode() {
		// If we're not an exit node, there's no point to
		// being a DNS server for somebody.
		return false
	}
	if !h.remoteAddr.IsValid() {
		// This should never be the case if the peerAPIHandler
		// was wired up correctly, but just in case.
		return false
	}
	// Otherwise, we're an exit node but the peer is not us, so
	// we need to check if they're allowed access to the internet.
	// As peerapi bypasses wgengine/filter checks, we need to check
	// ourselves. As a proxy for autogroup:internet access, we see
	// if we would've accepted a packet to 0.0.0.0:53. We treat
	// the IP 0.0.0.0 as being "the internet".
	f := b.filterAtomic.Load()
	if f == nil {
		return false
	}
	// Note: we check TCP here because the Filter type already had
	// a CheckTCP method (for unit tests), but it's pretty
	// arbitrary. DNS runs over TCP and UDP, so sure... we check
	// TCP.
	dstIP := netaddr.IPv4(0, 0, 0, 0)
	remoteIP := h.remoteAddr.Addr()
	if remoteIP.Is6() {
		// autogroup:internet for IPv6 is defined to start with 2000::/3,
		// so use 2000::0 as the probe "the internet" address.
		dstIP = netip.MustParseAddr("2000::")
	}
	verdict := f.CheckTCP(remoteIP, dstIP, 53)
	return verdict == filter.Accept
}

// handleDNSQuery implements a DoH server (RFC 8484) over the peerapi.
// It's not over HTTPS as the spec dictates, but rather HTTP-over-WireGuard.
func (h *peerAPIHandler) handleDNSQuery(w http.ResponseWriter, r *http.Request) {
	if h.ps.resolver == nil {
		http.Error(w, "DNS not wired up", http.StatusNotImplemented)
		return
	}
	if !h.replyToDNSQueries() {
		http.Error(w, "DNS access denied", http.StatusForbidden)
		return
	}
	pretty := false // non-DoH debug mode for humans
	q, publicError := dohQuery(r)
	if publicError != "" && r.Method == "GET" {
		if name := r.FormValue("q"); name != "" {
			pretty = true
			publicError = ""
			q = dnsQueryForName(name, r.FormValue("t"))
		}
	}
	if publicError != "" {
		http.Error(w, publicError, http.StatusBadRequest)
		return
	}

	// Some timeout that's short enough to be noticed by humans
	// but long enough that it's longer than real DNS timeouts.
	const arbitraryTimeout = 5 * time.Second

	ctx, cancel := context.WithTimeout(r.Context(), arbitraryTimeout)
	defer cancel()
	res, err := h.ps.resolver.HandleExitNodeDNSQuery(ctx, q, h.remoteAddr, h.ps.b.allowExitNodeDNSProxyToServeName)
	if err != nil {
		h.logf("handleDNS fwd error: %v", err)
		if err := ctx.Err(); err != nil {
			http.Error(w, err.Error(), 500)
		} else {
			http.Error(w, "DNS forwarding error", 500)
		}
		return
	}
	if pretty {
		// Non-standard response for interactive debugging.
		w.Header().Set("Content-Type", "application/json")
		writePrettyDNSReply(w, res)
		return
	}
	w.Header().Set("Content-Type", "application/dns-message")
	w.Header().Set("Content-Length", strconv.Itoa(len(res)))
	w.Write(res)
}

func dohQuery(r *http.Request) (dnsQuery []byte, publicErr string) {
	const maxQueryLen = 256 << 10
	switch r.Method {
	default:
		return nil, "bad HTTP method"
	case "GET":
		q64 := r.FormValue("dns")
		if q64 == "" {
			return nil, "missing 'dns' parameter"
		}
		if base64.RawURLEncoding.DecodedLen(len(q64)) > maxQueryLen {
			return nil, "query too large"
		}
		q, err := base64.RawURLEncoding.DecodeString(q64)
		if err != nil {
			return nil, "invalid 'dns' base64 encoding"
		}
		return q, ""
	case "POST":
		if r.Header.Get("Content-Type") != "application/dns-message" {
			return nil, "unexpected Content-Type"
		}
		q, err := io.ReadAll(io.LimitReader(r.Body, maxQueryLen+1))
		if err != nil {
			return nil, "error reading post body with DNS query"
		}
		if len(q) > maxQueryLen {
			return nil, "query too large"
		}
		return q, ""
	}
}

func dnsQueryForName(name, typStr string) []byte {
	typ := dnsmessage.TypeA
	switch strings.ToLower(typStr) {
	case "aaaa":
		typ = dnsmessage.TypeAAAA
	case "txt":
		typ = dnsmessage.TypeTXT
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		OpCode:           0, // query
		RecursionDesired: true,
		ID:               0,
	})
	if !strings.HasSuffix(name, ".") {
		name += "."
	}
	b.StartQuestions()
	b.Question(dnsmessage.Question{
		Name:  dnsmessage.MustNewName(name),
		Type:  typ,
		Class: dnsmessage.ClassINET,
	})
	msg, _ := b.Finish()
	return msg
}

func writePrettyDNSReply(w io.Writer, res []byte) (err error) {
	defer func() {
		if err != nil {
			j, _ := json.Marshal(struct {
				Error string
			}{err.Error()})
			j = append(j, '\n')
			w.Write(j)
			return
		}
	}()
	var p dnsmessage.Parser
	hdr, err := p.Start(res)
	if err != nil {
		return err
	}
	if hdr.RCode != dnsmessage.RCodeSuccess {
		return fmt.Errorf("DNS RCode = %v", hdr.RCode)
	}
	if err := p.SkipAllQuestions(); err != nil {
		return err
	}

	var gotIPs []string
	for {
		h, err := p.AnswerHeader()
		if err == dnsmessage.ErrSectionDone {
			break
		}
		if err != nil {
			return err
		}
		if h.Class != dnsmessage.ClassINET {
			continue
		}
		switch h.Type {
		case dnsmessage.TypeA:
			r, err := p.AResource()
			if err != nil {
				return err
			}
			gotIPs = append(gotIPs, net.IP(r.A[:]).String())
		case dnsmessage.TypeAAAA:
			r, err := p.AAAAResource()
			if err != nil {
				return err
			}
			gotIPs = append(gotIPs, net.IP(r.AAAA[:]).String())
		case dnsmessage.TypeTXT:
			r, err := p.TXTResource()
			if err != nil {
				return err
			}
			gotIPs = append(gotIPs, r.TXT...)
		}
	}
	j, _ := json.Marshal(gotIPs)
	j = append(j, '\n')
	w.Write(j)
	return nil
}

// newFakePeerAPIListener creates a new net.Listener that acts like
// it's listening on the provided IP address and on TCP port 1.
//
// See docs on fakePeerAPIListener.
func newFakePeerAPIListener(ip netip.Addr) net.Listener {
	return &fakePeerAPIListener{
		addr:   net.TCPAddrFromAddrPort(netip.AddrPortFrom(ip, 1)),
		closed: make(chan struct{}),
	}
}

// fakePeerAPIListener is a net.Listener that has an Addr method returning a TCPAddr
// for a given IP on port 1 (arbitrary) and can be Closed, but otherwise Accept
// just blocks forever until closed. The purpose of this is to let the rest
// of the LocalBackend/PeerAPI code run and think it's talking to the kernel,
// even if the kernel isn't cooperating (like on Android: Issue 4449, 4293, etc)
// or we lack permission to listen on a port. It's okay to not actually listen via
// the kernel because on almost all platforms (except iOS as of 2022-04-20) we
// also intercept netstack TCP requests in to our peerapi port and hand it over
// directly to peerapi, without involving the kernel. So this doesn't need to be
// real. But the port number we return (1, in this case) is the port number we advertise
// to peers and they connect to. 1 seems pretty safe to use. Even if the kernel's
// using it, it doesn't matter, as we intercept it first in netstack and the kernel
// never notices.
//
// Eventually we'll remove this code and do this on all platforms, when iOS also uses
// netstack.
type fakePeerAPIListener struct {
	addr net.Addr

	closeOnce sync.Once
	closed    chan struct{}
}

func (fl *fakePeerAPIListener) Close() error {
	fl.closeOnce.Do(func() { close(fl.closed) })
	return nil
}

func (fl *fakePeerAPIListener) Accept() (net.Conn, error) {
	<-fl.closed
	return nil, net.ErrClosed
}

func (fl *fakePeerAPIListener) Addr() net.Addr { return fl.addr }
