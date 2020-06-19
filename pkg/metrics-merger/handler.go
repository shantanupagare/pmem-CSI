/*
Copyright 2018,2019 reBuy reCommerce GmbH
Copyright 2020 Intel Corporation.

SPDX-License-Identifier: MIT
*/

package metricsmerger

import (
	"bytes"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	prom "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"k8s.io/klog"
	"net/http/httptest"
)

type Handler struct {
	Handlers             []http.Handler
	Exporters            []string
	ExportersHTTPTimeout int
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.Merge(w)
}

func (h Handler) Merge(w io.Writer) {
	mfs := map[string]*prom.MetricFamily{}

	responses := make([]map[string]*prom.MetricFamily, 1024)
	responsesMu := sync.Mutex{}
	httpClientTimeout := time.Second * time.Duration(h.ExportersHTTPTimeout)

	wg := sync.WaitGroup{}
	for _, url := range h.Exporters {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			httpClient := http.Client{Timeout: httpClientTimeout}
			resp, err := httpClient.Get(u)
			if err != nil {
				klog.Errorf("url %s: HTTP connection failed: %v", u, err)
				return
			}
			defer resp.Body.Close()

			tp := new(expfmt.TextParser)
			part, err := tp.TextToMetricFamilies(resp.Body)
			if err != nil {
				klog.Errorf("url %s: Parse response body to metrics: %v", u, err)
				return
			}
			responsesMu.Lock()
			responses = append(responses, part)
			responsesMu.Unlock()
		}(url)
	}
	for _, handler := range h.Handlers {
		wg.Add(1)
		handler := handler
		go func() {
			defer wg.Done()
			resp := httptest.ResponseRecorder{
				Body: &bytes.Buffer{},
			}
			handler.ServeHTTP(&resp, &http.Request{})
			if resp.Code != 200 {
				klog.Errorf("error code %d from metrics handler", resp.Code)
				return
			}
			tp := new(expfmt.TextParser)
			part, err := tp.TextToMetricFamilies(resp.Body)
			if err != nil {
				klog.Errorf("handler: parse response body to metrics: %v", err)
				return
			}
			responsesMu.Lock()
			responses = append(responses, part)
			responsesMu.Unlock()
		}()
	}
	wg.Wait()

	for _, part := range responses {
		for n, mf := range part {
			mfo, ok := mfs[n]
			if ok {
				mfo.Metric = append(mfo.Metric, mf.Metric...)
			} else {
				mfs[n] = mf
			}

		}
	}

	names := []string{}
	for n := range mfs {
		names = append(names, n)
	}
	sort.Strings(names)

	enc := expfmt.NewEncoder(w, expfmt.FmtText)
	for _, n := range names {
		err := enc.Encode(mfs[n])
		if err != nil {
			klog.Error(err)
			return
		}
	}
}
