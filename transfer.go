// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package httphelper

import (
	"bufio"
	"bytes"
	"fmt"
	"github.com/phuslu/httphelper/internal"
	"io"
	"io/ioutil"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

type errorReader struct {
	err error
}

func (r errorReader) Read(p []byte) (n int, err error) {
	return 0, r.err
}

func noBodyExpected(requestMethod string) bool {
	return requestMethod == "HEAD"
}

// Checks whether chunked is part of the encodings stack
func chunked(te []string) bool { return len(te) > 0 && te[0] == "chunked" }

// Checks whether the encoding is explicitly "identity".
func isIdentity(te []string) bool { return len(te) == 1 && te[0] == "identity" }

// transferWriter inspects the fields of a user-supplied Request or Response,
// sanitizes them without changing the user object and provides methods for
// writing the respective header, body and trailer in wire format.
type transferWriter struct {
	Method           string
	Body             io.Reader
	BodyCloser       io.Closer
	ResponseToHEAD   bool
	ContentLength    int64 // -1 means unknown, 0 means exactly none
	Close            bool
	TransferEncoding []string
	Trailer          http.Header
	IsResponse       bool
}

func newTransferWriter(r interface{}) (t *transferWriter, err error) {
	t = &transferWriter{}

	// Extract relevant fields
	atLeastHTTP11 := false
	switch rr := r.(type) {
	case *http.Request:
		if rr.ContentLength != 0 && rr.Body == nil {
			return nil, fmt.Errorf("http: Request.ContentLength=%d with nil Body", rr.ContentLength)
		}
		t.Method = rr.Method
		t.Body = rr.Body
		t.BodyCloser = rr.Body
		t.ContentLength = rr.ContentLength
		t.Close = rr.Close
		t.TransferEncoding = rr.TransferEncoding
		t.Trailer = rr.Trailer
		atLeastHTTP11 = rr.ProtoAtLeast(1, 1)
		if t.Body != nil && len(t.TransferEncoding) == 0 && atLeastHTTP11 {
			if t.ContentLength == 0 {
				// Test to see if it's actually zero or just unset.
				var buf [1]byte
				n, rerr := io.ReadFull(t.Body, buf[:])
				if rerr != nil && rerr != io.EOF {
					t.ContentLength = -1
					t.Body = errorReader{rerr}
				} else if n == 1 {
					// Oh, guess there is data in this Body Reader after all.
					// The ContentLength field just wasn't set.
					// Stich the Body back together again, re-attaching our
					// consumed byte.
					t.ContentLength = -1
					t.Body = io.MultiReader(bytes.NewReader(buf[:]), t.Body)
				} else {
					// Body is actually empty.
					t.Body = nil
					t.BodyCloser = nil
				}
			}
			if t.ContentLength < 0 {
				t.TransferEncoding = []string{"chunked"}
			}
		}
	case *http.Response:
		t.IsResponse = true
		if rr.Request != nil {
			t.Method = rr.Request.Method
		}
		t.Body = rr.Body
		t.BodyCloser = rr.Body
		t.ContentLength = rr.ContentLength
		t.Close = rr.Close
		t.TransferEncoding = rr.TransferEncoding
		t.Trailer = rr.Trailer
		atLeastHTTP11 = rr.ProtoAtLeast(1, 1)
		t.ResponseToHEAD = noBodyExpected(t.Method)
	}

	// Sanitize Body,ContentLength,TransferEncoding
	if t.ResponseToHEAD {
		t.Body = nil
		if chunked(t.TransferEncoding) {
			t.ContentLength = -1
		}
	} else {
		if !atLeastHTTP11 || t.Body == nil {
			t.TransferEncoding = nil
		}
		if chunked(t.TransferEncoding) {
			t.ContentLength = -1
		} else if t.Body == nil { // no chunking, no body
			t.ContentLength = 0
		}
	}

	// Sanitize Trailer
	if !chunked(t.TransferEncoding) {
		t.Trailer = nil
	}

	return t, nil
}

func (t *transferWriter) shouldSendContentLength() bool {
	if chunked(t.TransferEncoding) {
		return false
	}
	if t.ContentLength > 0 {
		return true
	}
	if t.ContentLength < 0 {
		return false
	}
	// Many servers expect a Content-Length for these methods
	if t.Method == "POST" || t.Method == "PUT" {
		return true
	}
	if t.ContentLength == 0 && isIdentity(t.TransferEncoding) {
		if t.Method == "GET" || t.Method == "HEAD" {
			return false
		}
		return true
	}

	return false
}

func (t *transferWriter) WriteHeader(w io.Writer) error {
	if t.Close {
		if _, err := io.WriteString(w, "Connection: close\r\n"); err != nil {
			return err
		}
	}

	// Write Content-Length and/or Transfer-Encoding whose values are a
	// function of the sanitized field triple (Body, ContentLength,
	// TransferEncoding)
	if t.shouldSendContentLength() {
		if _, err := io.WriteString(w, "Content-Length: "); err != nil {
			return err
		}
		if _, err := io.WriteString(w, strconv.FormatInt(t.ContentLength, 10)+"\r\n"); err != nil {
			return err
		}
	} else if chunked(t.TransferEncoding) {
		if _, err := io.WriteString(w, "Transfer-Encoding: chunked\r\n"); err != nil {
			return err
		}
	}

	// Write Trailer header
	if t.Trailer != nil {
		keys := make([]string, 0, len(t.Trailer))
		for k := range t.Trailer {
			k = http.CanonicalHeaderKey(k)
			switch k {
			case "Transfer-Encoding", "Trailer", "Content-Length":
				return &badStringError{"invalid Trailer key", k}
			}
			keys = append(keys, k)
		}
		if len(keys) > 0 {
			sort.Strings(keys)
			// TODO: could do better allocation-wise here, but trailers are rare,
			// so being lazy for now.
			if _, err := io.WriteString(w, "Trailer: "+strings.Join(keys, ",")+"\r\n"); err != nil {
				return err
			}
		}
	}

	return nil
}

func (t *transferWriter) WriteBody(w io.Writer) error {
	var err error
	var ncopy int64

	// Write body
	if t.Body != nil {
		if chunked(t.TransferEncoding) {
			if bw, ok := w.(*bufio.Writer); ok && !t.IsResponse {
				w = &internal.FlushAfterChunkWriter{bw}
			}
			cw := internal.NewChunkedWriter(w)
			_, err = io.Copy(cw, t.Body)
			if err == nil {
				err = cw.Close()
			}
		} else if t.ContentLength == -1 {
			ncopy, err = io.Copy(w, t.Body)
		} else {
			ncopy, err = io.Copy(w, io.LimitReader(t.Body, t.ContentLength))
			if err != nil {
				return err
			}
			var nextra int64
			nextra, err = io.Copy(ioutil.Discard, t.Body)
			ncopy += nextra
		}
		if err != nil {
			return err
		}
		if err = t.BodyCloser.Close(); err != nil {
			return err
		}
	}

	if !t.ResponseToHEAD && t.ContentLength != -1 && t.ContentLength != ncopy {
		return fmt.Errorf("http: ContentLength=%d with Body length %d",
			t.ContentLength, ncopy)
	}

	if chunked(t.TransferEncoding) {
		// Write Trailer header
		if t.Trailer != nil {
			if err := t.Trailer.Write(w); err != nil {
				return err
			}
		}
		// Last chunk, empty trailer
		_, err = io.WriteString(w, "\r\n")
	}
	return err
}
