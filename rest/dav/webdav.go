// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package webdav provides a WebDAV server implementation.
package dav

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

type Handler struct {
	// Prefix is the URL path prefix to strip from WebDAV resource paths.
	Prefix string
	// FileSystem is the virtual file system.
	FileSystem FileSystem
	// LockSystem is the lock management system.
	LockSystem LockSystem
	// Logger is an optional error logger. If non-nil, it will be called
	// for all HTTP requests.
	Logger func(*http.Request, error)
}

func (this *Handler) stripPrefix(p string) (string, int, error) {
	if this.Prefix == "" {
		return p, http.StatusOK, nil
	}
	if r := strings.TrimPrefix(p, this.Prefix); len(r) < len(p) {
		return r, http.StatusOK, nil
	}
	return p, http.StatusNotFound, errPrefixMismatch
}

func (this *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	status, err := http.StatusBadRequest, errUnsupportedMethod
	if this.FileSystem == nil {
		status, err = http.StatusInternalServerError, errNoFileSystem
	} else if this.LockSystem == nil {
		status, err = http.StatusInternalServerError, errNoLockSystem
	} else {
		switch r.Method {
		case "OPTIONS":
			status, err = this.handleOptions(w, r)
		case "GET", "HEAD", "POST":
			status, err = this.handleGetHeadPost(w, r)
		case "DELETE":
			status, err = this.handleDelete(w, r)
		case "PUT":
			status, err = this.handlePut(w, r)
		case "MKCOL":
			status, err = this.handleMkcol(w, r)
		case "COPY", "MOVE":
			status, err = this.handleCopyMove(w, r)
		case "LOCK":
			status, err = this.handleLock(w, r)
		case "UNLOCK":
			status, err = this.handleUnlock(w, r)
		case "PROPFIND":
			status, err = this.handlePropfind(w, r)
		case "PROPPATCH":
			status, err = this.handleProppatch(w, r)
		}
	}

	if status != 0 {
		w.WriteHeader(status)
		if status != http.StatusNoContent {
			w.Write([]byte(StatusText(status)))
		}
	}
	if this.Logger != nil {
		this.Logger(r, err)
	}
}

func (this *Handler) lock(now time.Time, root string) (token string, status int, err error) {
	token, err = this.LockSystem.Create(now, LockDetails{
		Root:      root,
		Duration:  infiniteTimeout,
		ZeroDepth: true,
	})
	if err != nil {
		if err == ErrLocked {
			return "", StatusLocked, err
		}
		return "", http.StatusInternalServerError, err
	}
	return token, 0, nil
}

func (this *Handler) confirmLocks(r *http.Request, src, dst string) (release func(), status int, err error) {
	hdr := r.Header.Get("If")
	if hdr == "" {
		// An empty If header means that the client hasn't previously created locks.
		// Even if this client doesn't care about locks, we still need to check that
		// the resources aren't locked by another client, so we create temporary
		// locks that would conflict with another client's locks. These temporary
		// locks are unlocked at the end of the HTTP request.
		now, srcToken, dstToken := time.Now(), "", ""
		if src != "" {
			srcToken, status, err = this.lock(now, src)
			if err != nil {
				return nil, status, err
			}
		}
		if dst != "" {
			dstToken, status, err = this.lock(now, dst)
			if err != nil {
				if srcToken != "" {
					this.LockSystem.Unlock(now, srcToken)
				}
				return nil, status, err
			}
		}

		return func() {
			if dstToken != "" {
				this.LockSystem.Unlock(now, dstToken)
			}
			if srcToken != "" {
				this.LockSystem.Unlock(now, srcToken)
			}
		}, 0, nil
	}

	ih, ok := parseIfHeader(hdr)
	if !ok {
		return nil, http.StatusBadRequest, errInvalidIfHeader
	}
	// ih is a disjunction (OR) of ifLists, so any ifList will do.
	for _, l := range ih.lists {
		lsrc := l.resourceTag
		if lsrc == "" {
			lsrc = src
		} else {
			u, err := url.Parse(lsrc)
			if err != nil {
				continue
			}
			if u.Host != r.Host {
				continue
			}
			lsrc, status, err = this.stripPrefix(u.Path)
			if err != nil {
				return nil, status, err
			}
		}
		release, err = this.LockSystem.Confirm(time.Now(), lsrc, dst, l.conditions...)
		if err == ErrConfirmationFailed {
			continue
		}
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		return release, 0, nil
	}
	// Section 10.4.1 says that "If this header is evaluated and all state lists
	// fail, then the request must fail with a 412 (Precondition Failed) status."
	// We follow the spec even though the cond_put_corrupt_token test case from
	// the litmus test warns on seeing a 412 instead of a 423 (Locked).
	return nil, http.StatusPreconditionFailed, ErrLocked
}

func (this *Handler) handleOptions(w http.ResponseWriter, r *http.Request) (status int, err error) {
	reqPath, status, err := this.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}
	ctx := r.Context()
	allow := "OPTIONS, LOCK, PUT, MKCOL"
	if fi, err := this.FileSystem.Stat(ctx, reqPath); err == nil {
		if fi.IsDir() {
			allow = "OPTIONS, LOCK, DELETE, PROPPATCH, COPY, MOVE, UNLOCK, PROPFIND"
		} else {
			allow = "OPTIONS, LOCK, GET, HEAD, POST, DELETE, PROPPATCH, COPY, MOVE, UNLOCK, PROPFIND, PUT"
		}
	}
	w.Header().Set("Allow", allow)
	// http://www.webdav.org/specs/rfc4918.html#dav.compliance.classes
	w.Header().Set("DAV", "1, 2")
	// http://msdn.microsoft.com/en-au/library/cc250217.aspx
	w.Header().Set("MS-Author-Via", "DAV")
	return 0, nil
}

func (this *Handler) handleGetHeadPost(w http.ResponseWriter, r *http.Request) (status int, err error) {
	reqPath, status, err := this.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}
	// TODO: check locks for read-only access??
	ctx := r.Context()
	f, err := this.FileSystem.OpenFile(ctx, reqPath, os.O_RDONLY, 0)
	if err != nil {
		return http.StatusNotFound, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return http.StatusNotFound, err
	}
	if fi.IsDir() {
		return http.StatusMethodNotAllowed, nil
	}
	etag, err := findETag(ctx, this.FileSystem, this.LockSystem, reqPath, fi)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	w.Header().Set("ETag", etag)
	// Let ServeContent determine the Content-Type header.
	http.ServeContent(w, r, reqPath, fi.ModTime(), f)
	return 0, nil
}

func (this *Handler) handleDelete(w http.ResponseWriter, r *http.Request) (status int, err error) {
	reqPath, status, err := this.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}
	release, status, err := this.confirmLocks(r, reqPath, "")
	if err != nil {
		return status, err
	}
	defer release()

	ctx := r.Context()

	// TODO: return MultiStatus where appropriate.

	// "godoc os RemoveAll" says that "If the path does not exist, RemoveAll
	// returns nil (no error)." WebDAV semantics are that it should return a
	// "404 Not Found". We therefore have to Stat before we RemoveAll.
	if _, err := this.FileSystem.Stat(ctx, reqPath); err != nil {
		if os.IsNotExist(err) {
			return http.StatusNotFound, err
		}
		return http.StatusMethodNotAllowed, err
	}
	if err := this.FileSystem.RemoveAll(ctx, reqPath); err != nil {
		return http.StatusMethodNotAllowed, err
	}
	return http.StatusNoContent, nil
}

func (this *Handler) handlePut(w http.ResponseWriter, r *http.Request) (status int, err error) {
	reqPath, status, err := this.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}
	release, status, err := this.confirmLocks(r, reqPath, "")
	if err != nil {
		return status, err
	}
	defer release()
	// TODO(rost): Support the If-Match, If-None-Match headers? See bradfitz'
	// comments in http.checkEtag.
	ctx := r.Context()

	f, err := this.FileSystem.OpenFile(ctx, reqPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		return http.StatusNotFound, err
	}
	_, copyErr := io.Copy(f, r.Body)
	fi, statErr := f.Stat()
	closeErr := f.Close()
	// TODO(rost): Returning 405 Method Not Allowed might not be appropriate.
	if copyErr != nil {
		return http.StatusMethodNotAllowed, copyErr
	}
	if statErr != nil {
		return http.StatusMethodNotAllowed, statErr
	}
	if closeErr != nil {
		return http.StatusMethodNotAllowed, closeErr
	}
	etag, err := findETag(ctx, this.FileSystem, this.LockSystem, reqPath, fi)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	w.Header().Set("ETag", etag)
	return http.StatusCreated, nil
}

func (this *Handler) handleMkcol(w http.ResponseWriter, r *http.Request) (status int, err error) {
	reqPath, status, err := this.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}
	release, status, err := this.confirmLocks(r, reqPath, "")
	if err != nil {
		return status, err
	}
	defer release()

	ctx := r.Context()

	if r.ContentLength > 0 {
		return http.StatusUnsupportedMediaType, nil
	}
	if err := this.FileSystem.Mkdir(ctx, reqPath, 0777); err != nil {
		if os.IsNotExist(err) {
			return http.StatusConflict, err
		}
		return http.StatusMethodNotAllowed, err
	}
	return http.StatusCreated, nil
}

func (this *Handler) handleCopyMove(w http.ResponseWriter, r *http.Request) (status int, err error) {
	hdr := r.Header.Get("Destination")
	if hdr == "" {
		return http.StatusBadRequest, errInvalidDestination
	}
	u, err := url.Parse(hdr)
	if err != nil {
		return http.StatusBadRequest, errInvalidDestination
	}
	if u.Host != r.Host {
		return http.StatusBadGateway, errInvalidDestination
	}

	src, status, err := this.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}

	dst, status, err := this.stripPrefix(u.Path)
	if err != nil {
		return status, err
	}

	if dst == "" {
		return http.StatusBadGateway, errInvalidDestination
	}
	if dst == src {
		return http.StatusForbidden, errDestinationEqualsSource
	}

	ctx := r.Context()

	if r.Method == "COPY" {
		// Section 7.5.1 says that a COPY only needs to lock the destination,
		// not both destination and source. Strictly speaking, this is racy,
		// even though a COPY doesn't modify the source, if a concurrent
		// operation modifies the source. However, the litmus test explicitly
		// checks that COPYing a locked-by-another source is OK.
		release, status, err := this.confirmLocks(r, "", dst)
		if err != nil {
			return status, err
		}
		defer release()

		// Section 9.8.3 says that "The COPY method on a collection without a Depth
		// header must act as if a Depth header with value "infinity" was included".
		depth := infiniteDepth
		if hdr := r.Header.Get("Depth"); hdr != "" {
			depth = parseDepth(hdr)
			if depth != 0 && depth != infiniteDepth {
				// Section 9.8.3 says that "A client may submit a Depth header on a
				// COPY on a collection with a value of "0" or "infinity"."
				return http.StatusBadRequest, errInvalidDepth
			}
		}
		return copyFiles(ctx, this.FileSystem, src, dst, r.Header.Get("Overwrite") != "F", depth, 0)
	}

	release, status, err := this.confirmLocks(r, src, dst)
	if err != nil {
		return status, err
	}
	defer release()

	// Section 9.9.2 says that "The MOVE method on a collection must act as if
	// a "Depth: infinity" header was used on it. A client must not submit a
	// Depth header on a MOVE on a collection with any value but "infinity"."
	if hdr := r.Header.Get("Depth"); hdr != "" {
		if parseDepth(hdr) != infiniteDepth {
			return http.StatusBadRequest, errInvalidDepth
		}
	}
	return moveFiles(ctx, this.FileSystem, src, dst, r.Header.Get("Overwrite") == "T")
}

func (this *Handler) handleLock(w http.ResponseWriter, r *http.Request) (retStatus int, retErr error) {
	duration, err := parseTimeout(r.Header.Get("Timeout"))
	if err != nil {
		return http.StatusBadRequest, err
	}
	li, status, err := ReadLockInfo(r.Body)
	if err != nil {
		return status, err
	}

	ctx := r.Context()
	token, ld, now, created := "", LockDetails{}, time.Now(), false
	if li == (LockInfo{}) {
		// An empty lockInfo means to refresh the lock.
		ih, ok := parseIfHeader(r.Header.Get("If"))
		if !ok {
			return http.StatusBadRequest, errInvalidIfHeader
		}
		if len(ih.lists) == 1 && len(ih.lists[0].conditions) == 1 {
			token = ih.lists[0].conditions[0].Token
		}
		if token == "" {
			return http.StatusBadRequest, errInvalidLockToken
		}
		ld, err = this.LockSystem.Refresh(now, token, duration)
		if err != nil {
			if err == ErrNoSuchLock {
				return http.StatusPreconditionFailed, err
			}
			return http.StatusInternalServerError, err
		}

	} else {
		// Section 9.10.3 says that "If no Depth header is submitted on a LOCK request,
		// then the request MUST act as if a "Depth:infinity" had been submitted."
		depth := infiniteDepth
		if hdr := r.Header.Get("Depth"); hdr != "" {
			depth = parseDepth(hdr)
			if depth != 0 && depth != infiniteDepth {
				// Section 9.10.3 says that "Values other than 0 or infinity must not be
				// used with the Depth header on a LOCK method".
				return http.StatusBadRequest, errInvalidDepth
			}
		}
		reqPath, status, err := this.stripPrefix(r.URL.Path)
		if err != nil {
			return status, err
		}
		ld = LockDetails{
			Root:      reqPath,
			Duration:  duration,
			OwnerXML:  li.Owner.InnerXML,
			ZeroDepth: depth == 0,
		}
		token, err = this.LockSystem.Create(now, ld)
		if err != nil {
			if err == ErrLocked {
				return StatusLocked, err
			}
			return http.StatusInternalServerError, err
		}
		defer func() {
			if retErr != nil {
				this.LockSystem.Unlock(now, token)
			}
		}()

		// Create the resource if it didn't previously exist.
		if _, err := this.FileSystem.Stat(ctx, reqPath); err != nil {
			f, err := this.FileSystem.OpenFile(ctx, reqPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
			if err != nil {
				// TODO: detect missing intermediate dirs and return http.StatusConflict?
				return http.StatusInternalServerError, err
			}
			f.Close()
			created = true
		}

		// http://www.webdav.org/specs/rfc4918.html#HEADER_Lock-Token says that the
		// Lock-Token value is a Coded-URL. We add angle brackets.
		w.Header().Set("Lock-Token", "<"+token+">")
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	if created {
		// This is "w.WriteHeader(http.StatusCreated)" and not "return
		// http.StatusCreated, nil" because we write our own (XML) response to w
		// and Handler.ServeHTTP would otherwise write "Created".
		w.WriteHeader(http.StatusCreated)
	}
	WriteLockInfo(w, token, ld)
	return 0, nil
}

func (this *Handler) handleUnlock(w http.ResponseWriter, r *http.Request) (status int, err error) {
	// http://www.webdav.org/specs/rfc4918.html#HEADER_Lock-Token says that the
	// Lock-Token value is a Coded-URL. We strip its angle brackets.
	t := r.Header.Get("Lock-Token")
	if len(t) < 2 || t[0] != '<' || t[len(t)-1] != '>' {
		return http.StatusBadRequest, errInvalidLockToken
	}
	t = t[1 : len(t)-1]

	switch err = this.LockSystem.Unlock(time.Now(), t); err {
	case nil:
		return http.StatusNoContent, err
	case ErrForbidden:
		return http.StatusForbidden, err
	case ErrLocked:
		return StatusLocked, err
	case ErrNoSuchLock:
		return http.StatusConflict, err
	default:
		return http.StatusInternalServerError, err
	}
}

func (this *Handler) handlePropfind(writer http.ResponseWriter, request *http.Request) (status int, err error) {
	reqPath, status, err := this.stripPrefix(request.URL.Path)
	if err != nil {
		return status, err
	}
	ctx := request.Context()
	fileInfo, err := this.FileSystem.Stat(ctx, reqPath)
	if err != nil {
		if os.IsNotExist(err) {
			return http.StatusNotFound, err
		}
		return http.StatusMethodNotAllowed, err
	}
	depth := infiniteDepth
	if hdr := request.Header.Get("Depth"); hdr != "" {
		depth = parseDepth(hdr)
		if depth == invalidDepth {
			return http.StatusBadRequest, errInvalidDepth
		}
	}
	//读取出request希望获取的文件属性。
	pf, status, err := ReadPropfind(request.Body)
	if err != nil {
		return status, err
	}

	multiStatusWriter := MultiStatusWriter{Writer: writer}

	walkFn := func(reqPath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		fmt.Printf("浏览：%s {name=%s,IsDir=%v,Mode=%v,ModTime=%v,Size=%v}\n",
			reqPath, info.Name(), info.IsDir(), info.Mode(), info.ModTime(), info.Size())
		var propstats []Propstat
		if pf.Propname != nil {
			pnames, err := Propnames(ctx, this.FileSystem, this.LockSystem, reqPath)
			if err != nil {
				return err
			}
			pstat := Propstat{Status: http.StatusOK}
			for _, xmlname := range pnames {
				pstat.Props = append(pstat.Props, Property{XMLName: xmlname})
			}
			propstats = append(propstats, pstat)
		} else if pf.Allprop != nil {
			propstats, err = allprop(ctx, this.FileSystem, this.LockSystem, reqPath, pf.Prop)
		} else {
			propstats, err = props(ctx, this.FileSystem, this.LockSystem, reqPath, pf.Prop)
		}
		if err != nil {
			return err
		}
		href := path.Join(this.Prefix, reqPath)
		if info.IsDir() {
			href += "/"
		}

		propstatResponse := makePropstatResponse(href, propstats)
		return multiStatusWriter.Write(propstatResponse)
	}

	walkErr := walkFS(ctx, this.FileSystem, depth, reqPath, fileInfo, walkFn)
	closeErr := multiStatusWriter.Close()
	if walkErr != nil {
		return http.StatusInternalServerError, walkErr
	}
	if closeErr != nil {
		return http.StatusInternalServerError, closeErr
	}
	return 0, nil
}

func (this *Handler) handleProppatch(w http.ResponseWriter, r *http.Request) (status int, err error) {
	reqPath, status, err := this.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}
	release, status, err := this.confirmLocks(r, reqPath, "")
	if err != nil {
		return status, err
	}
	defer release()

	ctx := r.Context()

	if _, err := this.FileSystem.Stat(ctx, reqPath); err != nil {
		if os.IsNotExist(err) {
			return http.StatusNotFound, err
		}
		return http.StatusMethodNotAllowed, err
	}
	patches, status, err := ReadProppatch(r.Body)
	if err != nil {
		return status, err
	}
	pstats, err := patch(ctx, this.FileSystem, this.LockSystem, reqPath, patches)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	mw := MultiStatusWriter{Writer: w}
	writeErr := mw.Write(makePropstatResponse(r.URL.Path, pstats))
	closeErr := mw.Close()
	if writeErr != nil {
		return http.StatusInternalServerError, writeErr
	}
	if closeErr != nil {
		return http.StatusInternalServerError, closeErr
	}
	return 0, nil
}

func makePropstatResponse(href string, pstats []Propstat) *Response {
	resp := Response{
		Href:     []string{(&url.URL{Path: href}).EscapedPath()},
		Propstat: make([]SubPropstat, 0, len(pstats)),
	}
	for _, p := range pstats {
		var xmlErr *XmlError
		if p.XMLError != "" {
			xmlErr = &XmlError{InnerXML: []byte(p.XMLError)}
		}
		resp.Propstat = append(resp.Propstat, SubPropstat{
			Status:              fmt.Sprintf("HTTP/1.1 %d %s", p.Status, StatusText(p.Status)),
			Prop:                p.Props,
			ResponseDescription: p.ResponseDescription,
			Error:               xmlErr,
		})
	}
	return &resp
}

const (
	infiniteDepth = -1
	invalidDepth  = -2
)

// parseDepth maps the strings "0", "1" and "infinity" to 0, 1 and
// infiniteDepth. Parsing any other string returns invalidDepth.
//
// Different WebDAV methods have further constraints on valid depths:
//	- PROPFIND has no further restrictions, as per section 9.1.
//	- COPY accepts only "0" or "infinity", as per section 9.8.3.
//	- MOVE accepts only "infinity", as per section 9.9.2.
//	- LOCK accepts only "0" or "infinity", as per section 9.10.3.
// These constraints are enforced by the handleXxx methods.
func parseDepth(s string) int {
	switch s {
	case "0":
		return 0
	case "1":
		return 1
	case "infinity":
		return infiniteDepth
	}
	return invalidDepth
}

// http://www.webdav.org/specs/rfc4918.html#status.code.extensions.to.http11
const (
	StatusMulti               = 207
	StatusUnprocessableEntity = 422
	StatusLocked              = 423
	StatusFailedDependency    = 424
	StatusInsufficientStorage = 507
)

func StatusText(code int) string {
	switch code {
	case StatusMulti:
		return "Multi-Status"
	case StatusUnprocessableEntity:
		return "Unprocessable Entity"
	case StatusLocked:
		return "Locked"
	case StatusFailedDependency:
		return "Failed Dependency"
	case StatusInsufficientStorage:
		return "Insufficient Storage"
	}
	return http.StatusText(code)
}

var (
	errDestinationEqualsSource = errors.New("webdav: destination equals source")
	errDirectoryNotEmpty       = errors.New("webdav: directory not empty")
	errInvalidDepth            = errors.New("webdav: invalid depth")
	errInvalidDestination      = errors.New("webdav: invalid destination")
	errInvalidIfHeader         = errors.New("webdav: invalid If header")
	errInvalidLockInfo         = errors.New("webdav: invalid lock info")
	errInvalidLockToken        = errors.New("webdav: invalid lock token")
	errInvalidPropfind         = errors.New("webdav: invalid propfind")
	errInvalidProppatch        = errors.New("webdav: invalid proppatch")
	errInvalidResponse         = errors.New("webdav: invalid response")
	errInvalidTimeout          = errors.New("webdav: invalid timeout")
	errNoFileSystem            = errors.New("webdav: no file system")
	errNoLockSystem            = errors.New("webdav: no lock system")
	errNotADirectory           = errors.New("webdav: not a directory")
	errPrefixMismatch          = errors.New("webdav: prefix mismatch")
	errRecursionTooDeep        = errors.New("webdav: recursion too deep")
	errUnsupportedLockInfo     = errors.New("webdav: unsupported lock info")
	errUnsupportedMethod       = errors.New("webdav: unsupported method")
)