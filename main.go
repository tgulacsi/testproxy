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
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"time"

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
	primary, secondary *http.Client
	dir                string
	id                 uint32
}

var timeout = 5 * time.Minute

func newDualServer(dir, primary, secondary string) *dualServer {
	tr := http.Transport{MaxIdleConnsPerHost: 4, ResponseHeaderTimeout: 30 * time.Second}
	return &dualServer{
		dir:       dir,
		primary:   &http.Client{Timeout: timeout, Transport: &tr},
		secondary: &http.Client{Timeout: timeout, Transport: &tr},
	}
}

func (ds *dualServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// TODO(tgulacsi): save just the headers and return a TeeWriter body.
	readReq, saveResp, err := ds.saveRequestResponse(r)
	if err != nil {
		Log.Crit("error saving request", "error", err)
		http.Error(w, fmt.Sprintf("error saving request: %v", err), http.StatusInternalServerError)
		return
	}

	if r, err = readReq(); err != nil {
		Log.Error("error reading request", "error", err)
		http.Error(w, fmt.Sprintf("error reading request: %v", err), http.StatusInternalServerError)
		return
	}
	r.RequestURI = ""
	resp1, err := ds.primary.Do(r)
	if err != nil {
		Log.Error("call primary", "error", err)
		http.Error(w, fmt.Sprintf("call primary: %v", err), http.StatusInternalServerError)
		return
	}
	go func(sc int) {
		if r, err = readReq(); err != nil {
			Log.Error("error reading request (2.)", "error", err)
			return
		}
		resp2, err := ds.secondary.Do(r)
		if err != nil {
			Log.Error("call secondary", "error", err)
			return
		}
		if _, err = saveResp(resp2, 2); err != nil {
			Log.Error("save secondary response", "error", err)
			return
		}

		if resp2.StatusCode != sc {
			Log.Warn("status code mismatch", "primary", sc, "secondary", resp2.StatusCode)
		}
	}(resp1.StatusCode)

	// TODO(tgulacsi): save just the headers and return a TeeWriter body.
	resp, err := saveResp(resp1, 1)
	if err != nil {
		Log.Error("save response1", "error", err)
		resp = resp1
	}

	// answer
	h := w.Header()
	for k, v := range resp.Header {
		h[k] = v
	}
	w.WriteHeader(resp.StatusCode)

	if _, err = io.Copy(w, resp.Body); err != nil {
		Log.Error("writing response", "error", err)
	}

}

func (ds *dualServer) saveRequestResponse(r *http.Request) (func() (*http.Request, error), func(*http.Response, int) (*http.Response, error), error) {
	id := ds.nextID()
	base := filepath.Join(ds.dir, fmt.Sprintf("%09d", id))
	saveReq, err := saveRequest(base+".0", r)
	if err != nil {
		return nil, nil, err
	}
	return saveReq, func(resp *http.Response, n int) (*http.Response, error) {
		if n <= 0 {
			panic("n must be bigger than zero!")
		}
		return saveResponse(base+fmt.Sprintf(".%d", n), resp)
	}, nil
}

func saveResponse(dest string, resp *http.Response) (*http.Response, error) {
	fh, err := os.Create(dest)
	if err != nil {
		return resp, err
	}
	if err = resp.Write(fh); err != nil {
		_ = fh.Close()
		return nil, err
	}
	if _, err = fh.Seek(0, 0); err != nil {
		_ = fh.Close()
		return nil, err
	}
	resp, err = http.ReadResponse(bufio.NewReader(fh), nil)
	if err != nil {
		_ = fh.Close()
		return resp, err
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{resp.Body, multiCloser{[]io.Closer{resp.Body, fh}}}
	return resp, nil
}

func saveRequest(dest string, r *http.Request) (func() (*http.Request, error), error) {
	fh, err := os.Create(dest)
	if err != nil {
		return nil, err
	}
	if err = r.Write(fh); err != nil {
		_ = fh.Close()
		return nil, err
	}
	if err = fh.Close(); err != nil {
		return nil, err
	}
	nm := fh.Name()
	return func() (*http.Request, error) {
		fh, err := os.Open(nm)
		if err != nil {
			return nil, err
		}
		req, err := http.ReadRequest(bufio.NewReader(fh))
		if err != nil {
			return req, err
		}
		req.Body = struct {
			io.Reader
			io.Closer
		}{req.Body, multiCloser{[]io.Closer{req.Body, fh}}}
		return req, nil
	}, nil
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
