// Copyright 2015 Tamás Gulácsi
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.

package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/tgulacsi/go/loghlp"
	"gopkg.in/inconshreveable/log15.v2"
)

var Log = log15.New()

func main() {
	hndl := log15.CallerFileHandler(log15.StderrHandler)
	Log.SetHandler(hndl)

	flagVerbose := flag.Bool("v", false, "should every proxy request be logged to stdout")
	flagHTTP := flag.String("http", ":8080", "on which address should the proxy listen")
	flagDir := flag.String("dir", "reqlog", "directory to log requests into")
	flag.Parse()

	if !*flagVerbose {
		hndl = log15.LvlFilterHandler(log15.LvlInfo, hndl)
		Log.SetHandler(hndl)
	}

	if err := os.MkdirAll(*flagDir, 0755); err != nil {
		Log.Crit("Can't create dir", "path", *flagDir, "error", err)
		os.Exit(1)
	}
	l, err := net.Listen("tcp", *flagHTTP)
	if err != nil {
		Log.Crit("Listen", "address", *flagHTTP, "error", err)
	}
	sl := newStoppableListener(l)
	ch := make(chan os.Signal)
	signal.Notify(ch, os.Interrupt)
	go func() {
		<-ch
		Log.Warn("Got SIGINT exiting")
		sl.Add(1)
		sl.Close()
		sl.Done()
	}()
	Log.Info("Starting Proxy, listening on " + *flagHTTP)
	http.Serve(sl, newDualServer(*flagDir, flag.Arg(0), flag.Arg(1)))
	sl.Wait()
	Log.Info("All connections closed - exit")
}

var _ = http.Handler(&dualServer{})

type dualServer struct {
	primary, secondary http.Handler
	dir                string
	id                 uint32
}

var timeout = 5 * time.Minute

func newDualServer(dir, primary, secondary string) *dualServer {
	lgr := loghlp.AsStdLog(Log.New(), log15.LvlError)
	tr := http.Transport{MaxIdleConnsPerHost: 4, ResponseHeaderTimeout: 30 * time.Second}
	priURL, err := url.Parse(primary)
	if err != nil {
		panic(err)
	}
	secURL, err := url.Parse(secondary)
	if err != nil {
		panic(err)
	}
	pri := httputil.NewSingleHostReverseProxy(priURL)
	pri.Transport, pri.ErrorLog = &tr, lgr
	sec := httputil.NewSingleHostReverseProxy(secURL)
	sec.Transport, sec.ErrorLog = &tr, lgr
	return &dualServer{
		dir:       dir,
		primary:   pri,
		secondary: sec,
	}
}

func (ds *dualServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requests, respWriters, err := ds.saveRequestResponse(r, 2)
	if err != nil {
		Log.Crit("error saving request", "error", err)
		http.Error(w, fmt.Sprintf("error saving request: %v", err), http.StatusInternalServerError)
		return
	}

	respWriters[0].w = w
	defer respWriters[0].WriteCloser.Close()

	Log.Debug("request 1", "req", requests[0])
	ds.primary.ServeHTTP(respWriters[0], requests[0])
	go func(w *respWriter, r *http.Request, sc int) {
		defer w.WriteCloser.Close()
		ds.secondary.ServeHTTP(w, r)
		if w.status != sc {
			Log.Error("CODE mismatch", "wanted", sc, "got", w.status)
		} else {
			base := w.WriteCloser.(*os.File).Name()
			base = base[:len(base)-1]
			for i := 0; i < 3; i++ {
				_ = os.Remove(base + strconv.Itoa(i))
			}
		}
	}(respWriters[1], requests[1], respWriters[0].status)
}

func (ds *dualServer) saveRequestResponse(r *http.Request, n int) ([]*http.Request, []*respWriter, error) {
	id := ds.nextID()
	base := filepath.Join(ds.dir, fmt.Sprintf("%09d", id))
	reqFn := base + ".0"
	if err := writeRequest(reqFn, r); err != nil {
		return nil, nil, err
	}
	requests := make([]*http.Request, n)
	respWriters := make([]*respWriter, n)
	for i := range requests {
		fh, err := os.Open(reqFn)
		if err != nil {
			return requests, respWriters, err
		}
		if requests[i], err = http.ReadRequest(bufio.NewReader(fh)); err != nil {
			return requests, respWriters, err
		}
		requests[i].Body = struct {
			io.Reader
			io.Closer
		}{requests[i].Body, multiCloser{[]io.Closer{requests[i].Body, fh}}}

		respWriters[i] = new(respWriter)
		if respWriters[i].WriteCloser, err = os.Create(
			base + fmt.Sprintf(".%d", i+1),
		); err != nil {
			return requests, respWriters, err
		}
	}

	return requests, respWriters, nil
}

var _ = http.ResponseWriter(&respWriter{})

type respWriter struct {
	io.WriteCloser
	w      http.ResponseWriter
	h      http.Header
	status int
}

func (w *respWriter) Header() http.Header {
	if w.w != nil {
		return w.w.Header()
	}
	if w.h == nil {
		w.h = make(http.Header, 8)
	}
	return w.h
}
func (w *respWriter) WriteHeader(status int) {
	w.status = status
	if w.w != nil {
		w.w.WriteHeader(status)
	}
	fmt.Fprintf(w.WriteCloser, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status))
	for k, vv := range w.Header() {
		for _, v := range vv {
			fmt.Fprintf(w.WriteCloser, "%s: %s\r\n", k, v)
		}
	}
	io.WriteString(w.WriteCloser, "\r\n")
}
func (w respWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.w != nil {
		n, err := w.w.Write(p)
		w.WriteCloser.Write(p)
		return n, err
	}
	return w.WriteCloser.Write(p)
}

func writeRequest(dest string, r *http.Request) error {
	fh, err := os.Create(dest)
	if err != nil {
		return err
	}
	if err = r.Write(fh); err != nil {
		_ = fh.Close()
		return err
	}
	return fh.Close()
}

func (ds *dualServer) nextID() uint32 {
	return atomic.AddUint32(&ds.id, 1)
}

type multiCloser struct {
	closers []io.Closer
}

func (mc multiCloser) Close() error {
	var err error
	for _, c := range mc.closers {
		if closeErr := c.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	return err
}
