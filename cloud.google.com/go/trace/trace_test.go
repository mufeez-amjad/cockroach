// Copyright 2016 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package trace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/internal/testutil"
	"cloud.google.com/go/storage"
	"golang.org/x/net/context"
	api "google.golang.org/api/cloudtrace/v1"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	dspb "google.golang.org/genproto/googleapis/datastore/v1"
	"google.golang.org/grpc"
)

const testProjectID = "testproject"

type fakeRoundTripper struct {
	reqc chan *http.Request
}

func newFakeRoundTripper() *fakeRoundTripper {
	return &fakeRoundTripper{reqc: make(chan *http.Request)}
}

func (rt *fakeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.reqc <- r
	resp := &http.Response{
		Status:     "200 OK",
		StatusCode: 200,
		Body:       ioutil.NopCloser(strings.NewReader("{}")),
	}
	return resp, nil
}

func newTestClient(rt http.RoundTripper) *Client {
	t, err := NewClient(context.Background(), testProjectID, option.WithHTTPClient(&http.Client{Transport: rt}))
	if err != nil {
		panic(err)
	}
	return t
}

type fakeDatastoreServer struct {
	dspb.DatastoreServer
	fail bool
}

func (f *fakeDatastoreServer) Lookup(ctx context.Context, req *dspb.LookupRequest) (*dspb.LookupResponse, error) {
	if f.fail {
		return nil, errors.New("failed!")
	}
	return &dspb.LookupResponse{}, nil
}

// makeRequests makes some requests.
// span is the root span.  rt is the trace client's http client's transport.
// This is used to retrieve the trace uploaded by the client, if any.  If
// expectTrace is true, we expect a trace will be uploaded.  If synchronous is
// true, the call to Finish is expected not to return before the client has
// uploaded any traces.
func makeRequests(t *testing.T, span *Span, rt *fakeRoundTripper, synchronous bool, expectTrace bool) *http.Request {
	ctx := NewContext(context.Background(), span)

	// An HTTP request.
	{
		req2, err := http.NewRequest("GET", "http://example.com/bar", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp := &http.Response{StatusCode: 200}
		s := span.NewRemoteChild(req2)
		s.Finish(WithResponse(resp))
	}

	// An autogenerated API call.
	{
		rt := &fakeRoundTripper{reqc: make(chan *http.Request, 1)}
		hc := &http.Client{Transport: rt}
		computeClient, err := compute.New(hc)
		if err != nil {
			t.Fatal(err)
		}
		_, err = computeClient.Zones.List(testProjectID).Context(ctx).Do()
		if err != nil {
			t.Fatal(err)
		}
	}

	// A cloud library call that uses the autogenerated API.
	{
		rt := &fakeRoundTripper{reqc: make(chan *http.Request, 1)}
		hc := &http.Client{Transport: rt}
		storageClient, err := storage.NewClient(context.Background(), option.WithHTTPClient(hc))
		if err != nil {
			t.Fatal(err)
		}
		var objAttrsList []*storage.ObjectAttrs
		it := storageClient.Bucket("testbucket").Objects(ctx, nil)
		for {
			objAttrs, err := it.Next()
			if err != nil && err != iterator.Done {
				t.Fatal(err)
			}
			if err == iterator.Done {
				break
			}
			objAttrsList = append(objAttrsList, objAttrs)
		}
	}

	// A cloud library call that uses grpc internally.
	for _, fail := range []bool{false, true} {
		srv, err := testutil.NewServer()
		if err != nil {
			t.Fatalf("creating test datastore server: %v", err)
		}
		dspb.RegisterDatastoreServer(srv.Gsrv, &fakeDatastoreServer{fail: fail})
		srv.Start()
		conn, err := grpc.Dial(srv.Addr, grpc.WithInsecure(), EnableGRPCTracingDialOption)
		if err != nil {
			t.Fatalf("connecting to test datastore server: %v", err)
		}
		datastoreClient, err := datastore.NewClient(ctx, testProjectID, option.WithGRPCConn(conn))
		if err != nil {
			t.Fatalf("creating datastore client: %v", err)
		}
		k := datastore.NameKey("Entity", "stringID", nil)
		e := new(datastore.Entity)
		datastoreClient.Get(ctx, k, e)
	}

	done := make(chan struct{})
	go func() {
		if synchronous {
			err := span.FinishWait()
			if err != nil {
				t.Errorf("Unexpected error from span.FinishWait: %v", err)
			}
		} else {
			span.Finish()
		}
		done <- struct{}{}
	}()
	if !expectTrace {
		<-done
		select {
		case <-rt.reqc:
			t.Errorf("Got a trace, expected none.")
		case <-time.After(5 * time.Millisecond):
		}
		return nil
	} else if !synchronous {
		<-done
		return <-rt.reqc
	} else {
		select {
		case <-done:
			t.Errorf("Synchronous Finish didn't wait for trace upload.")
			return <-rt.reqc
		case <-time.After(5 * time.Millisecond):
			r := <-rt.reqc
			<-done
			return r
		}
	}
}

func TestTrace(t *testing.T) {
	t.Parallel()
	testTrace(t, false, true)
}

func TestTraceWithWait(t *testing.T) {
	testTrace(t, true, true)
}

func TestTraceFromHeader(t *testing.T) {
	t.Parallel()
	testTrace(t, false, false)
}

func TestTraceFromHeaderWithWait(t *testing.T) {
	testTrace(t, false, true)
}

func testTrace(t *testing.T, synchronous bool, fromRequest bool) {
	const header = `0123456789ABCDEF0123456789ABCDEF/42;o=3`
	rt := newFakeRoundTripper()
	traceClient := newTestClient(rt)
	var span *Span
	if fromRequest {
		req, err := http.NewRequest("GET", "http://example.com/foo", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Cloud-Trace-Context", header)
		span = traceClient.SpanFromRequest(req)
	} else {
		span = traceClient.SpanFromHeader("/foo", header)
	}
	uploaded := makeRequests(t, span, rt, synchronous, true)

	if uploaded == nil {
		t.Fatalf("No trace uploaded, expected one.")
	}

	var expectedServerLabels map[string]string
	if fromRequest {
		expectedServerLabels = map[string]string{
			"trace.cloud.google.com/http/host":   "example.com",
			"trace.cloud.google.com/http/method": "GET",
			"trace.cloud.google.com/http/url":    "http://example.com/foo",
		}
	} else {
		expectedServerLabels = map[string]string{}
	}

	expected := api.Traces{
		Traces: []*api.Trace{
			{
				ProjectId: testProjectID,
				Spans: []*api.TraceSpan{
					{
						Kind: "RPC_CLIENT",
						Labels: map[string]string{
							"trace.cloud.google.com/http/host":        "example.com",
							"trace.cloud.google.com/http/method":      "GET",
							"trace.cloud.google.com/http/status_code": "200",
							"trace.cloud.google.com/http/url":         "http://example.com/bar",
						},
						Name: "/bar",
					},
					{
						Kind: "RPC_CLIENT",
						Labels: map[string]string{
							"trace.cloud.google.com/http/host":        "www.googleapis.com",
							"trace.cloud.google.com/http/method":      "GET",
							"trace.cloud.google.com/http/status_code": "200",
							"trace.cloud.google.com/http/url":         "https://www.googleapis.com/compute/v1/projects/testproject/zones",
						},
						Name: "/compute/v1/projects/testproject/zones",
					},
					{
						Kind: "RPC_CLIENT",
						Labels: map[string]string{
							"trace.cloud.google.com/http/host":        "www.googleapis.com",
							"trace.cloud.google.com/http/method":      "GET",
							"trace.cloud.google.com/http/status_code": "200",
							"trace.cloud.google.com/http/url":         "https://www.googleapis.com/storage/v1/b/testbucket/o",
						},
						Name: "/storage/v1/b/testbucket/o",
					},
					&api.TraceSpan{
						Kind:   "RPC_CLIENT",
						Labels: nil,
						Name:   "/google.datastore.v1.Datastore/Lookup",
					},
					&api.TraceSpan{
						Kind:   "RPC_CLIENT",
						Labels: map[string]string{"error": "rpc error: code = 2 desc = failed!"},
						Name:   "/google.datastore.v1.Datastore/Lookup",
					},
					{
						Kind:   "RPC_SERVER",
						Labels: expectedServerLabels,
						Name:   "/foo",
					},
				},
				TraceId: "0123456789ABCDEF0123456789ABCDEF",
			},
		},
	}

	body, err := ioutil.ReadAll(uploaded.Body)
	if err != nil {
		t.Fatal(err)
	}
	var patch api.Traces
	err = json.Unmarshal(body, &patch)
	if err != nil {
		t.Fatal(err)
	}

	if len(patch.Traces) != len(expected.Traces) || len(patch.Traces[0].Spans) != len(expected.Traces[0].Spans) {
		got, _ := json.Marshal(patch)
		want, _ := json.Marshal(expected)
		t.Fatalf("PatchTraces request: got %s want %s", got, want)
	}

	n := len(patch.Traces[0].Spans)
	rootSpan := patch.Traces[0].Spans[n-1]
	for i, s := range patch.Traces[0].Spans {
		if a, b := s.StartTime, s.EndTime; a > b {
			t.Errorf("span %d start time is later than its end time (%q, %q)", i, a, b)
		}
		if a, b := rootSpan.StartTime, s.StartTime; a > b {
			t.Errorf("trace start time is later than span %d start time (%q, %q)", i, a, b)
		}
		if a, b := s.EndTime, rootSpan.EndTime; a > b {
			t.Errorf("span %d end time is later than trace end time (%q, %q)", i, a, b)
		}
		if i > 1 && i < n-1 {
			if a, b := patch.Traces[0].Spans[i-1].EndTime, s.StartTime; a > b {
				t.Errorf("span %d end time is later than span %d start time (%q, %q)", i-1, i, a, b)
			}
		}
	}

	if x := rootSpan.ParentSpanId; x != 42 {
		t.Errorf("Incorrect ParentSpanId: got %d want %d", x, 42)
	}
	for i, s := range patch.Traces[0].Spans {
		if x, y := rootSpan.SpanId, s.ParentSpanId; i < n-1 && x != y {
			t.Errorf("Incorrect ParentSpanId in span %d: got %d want %d", i, y, x)
		}
	}
	for i, s := range patch.Traces[0].Spans {
		s.EndTime = ""
		labels := &expected.Traces[0].Spans[i].Labels
		for key, value := range *labels {
			if v, ok := s.Labels[key]; !ok {
				t.Errorf("Span %d is missing Label %q:%q", i, key, value)
			} else if key == "trace.cloud.google.com/http/url" {
				if !strings.HasPrefix(v, value) {
					t.Errorf("Span %d Label %q: got value %q want prefix %q", i, key, v, value)
				}
			} else if v != value {
				t.Errorf("Span %d Label %q: got value %q want %q", i, key, v, value)
			}
		}
		for key := range s.Labels {
			if _, ok := (*labels)[key]; key != "trace.cloud.google.com/stacktrace" && !ok {
				t.Errorf("Span %d: unexpected label %q", i, key)
			}
		}
		*labels = nil
		s.Labels = nil
		s.ParentSpanId = 0
		if s.SpanId == 0 {
			t.Errorf("Incorrect SpanId: got 0 want nonzero")
		}
		s.SpanId = 0
		s.StartTime = ""
	}
	if !reflect.DeepEqual(patch, expected) {
		got, _ := json.Marshal(patch)
		want, _ := json.Marshal(expected)
		t.Errorf("PatchTraces request: got %s want %s", got, want)
	}
}

func TestNoTrace(t *testing.T) {
	testNoTrace(t, false, true)
}

func TestNoTraceWithWait(t *testing.T) {
	testNoTrace(t, true, true)
}

func TestNoTraceFromHeader(t *testing.T) {
	testNoTrace(t, false, false)
}

func TestNoTraceFromHeaderWithWait(t *testing.T) {
	testNoTrace(t, true, false)
}

func testNoTrace(t *testing.T, synchronous bool, fromRequest bool) {
	for _, header := range []string{
		`0123456789ABCDEF0123456789ABCDEF/42;o=2`,
		`0123456789ABCDEF0123456789ABCDEF/42;o=0`,
		`0123456789ABCDEF0123456789ABCDEF/42`,
		`0123456789ABCDEF0123456789ABCDEF`,
		``,
	} {
		rt := newFakeRoundTripper()
		traceClient := newTestClient(rt)
		var span *Span
		if fromRequest {
			req, err := http.NewRequest("GET", "http://example.com/foo", nil)
			if header != "" {
				req.Header.Set("X-Cloud-Trace-Context", header)
			}
			if err != nil {
				t.Fatal(err)
			}
			span = traceClient.SpanFromRequest(req)
		} else {
			span = traceClient.SpanFromHeader("/foo", header)
		}
		uploaded := makeRequests(t, span, rt, synchronous, false)
		if uploaded != nil {
			t.Errorf("Got a trace, expected none.")
		}
	}
}

func TestSample(t *testing.T) {
	// A deterministic test of the sampler logic.
	type testCase struct {
		rate   float64
		maxqps float64
		want   int
	}
	const delta = 25 * time.Millisecond
	for _, test := range []testCase{
		// qps won't matter, so we will sample half of the 79 calls
		{0.50, 100, 40},
		// with 1 qps and a burst of 2, we will sample twice in second #1, once in the partial second #2
		{0.50, 1, 3},
	} {
		sp, err := NewLimitedSampler(test.rate, test.maxqps)
		if err != nil {
			t.Fatal(err)
		}
		s := sp.(*sampler)
		sampled := 0
		tm := time.Now()
		for i := 0; i < 80; i++ {
			if s.sample(Parameters{}, tm, float64(i%2)).Sample {
				sampled++
			}
			tm = tm.Add(delta)
		}
		if sampled != test.want {
			t.Errorf("rate=%f, maxqps=%f: got %d samples, want %d", test.rate, test.maxqps, sampled, test.want)
		}
	}
}

func TestSampling(t *testing.T) {
	t.Parallel()
	// This scope tests sampling in a larger context, with real time and randomness.
	wg := sync.WaitGroup{}
	type testCase struct {
		rate          float64
		maxqps        float64
		expectedRange [2]int
	}
	for _, test := range []testCase{
		{0, 5, [2]int{0, 0}},
		{5, 0, [2]int{0, 0}},
		{0.50, 100, [2]int{20, 60}},
		{0.50, 1, [2]int{3, 4}}, // Windows, with its less precise clock, sometimes gives 4.
	} {
		wg.Add(1)
		go func(test testCase) {
			rt := newFakeRoundTripper()
			traceClient := newTestClient(rt)
			traceClient.bundler.BundleByteLimit = 1
			p, err := NewLimitedSampler(test.rate, test.maxqps)
			if err != nil {
				t.Fatalf("NewLimitedSampler: %v", err)
			}
			traceClient.SetSamplingPolicy(p)
			ticker := time.NewTicker(25 * time.Millisecond)
			sampled := 0
			for i := 0; i < 79; i++ {
				req, err := http.NewRequest("GET", "http://example.com/foo", nil)
				if err != nil {
					t.Fatal(err)
				}
				span := traceClient.SpanFromRequest(req)
				span.Finish()
				select {
				case <-rt.reqc:
					<-ticker.C
					sampled++
				case <-ticker.C:
				}
			}
			ticker.Stop()
			if test.expectedRange[0] > sampled || sampled > test.expectedRange[1] {
				t.Errorf("rate=%f, maxqps=%f: got %d samples want ∈ %v", test.rate, test.maxqps, sampled, test.expectedRange)
			}
			wg.Done()
		}(test)
	}
	wg.Wait()
}

func TestBundling(t *testing.T) {
	t.Parallel()
	rt := newFakeRoundTripper()
	traceClient := newTestClient(rt)
	traceClient.bundler.DelayThreshold = time.Second / 2
	traceClient.bundler.BundleCountThreshold = 10
	p, err := NewLimitedSampler(1, 99) // sample every request.
	if err != nil {
		t.Fatalf("NewLimitedSampler: %v", err)
	}
	traceClient.SetSamplingPolicy(p)

	for i := 0; i < 35; i++ {
		go func() {
			req, err := http.NewRequest("GET", "http://example.com/foo", nil)
			if err != nil {
				t.Fatal(err)
			}
			span := traceClient.SpanFromRequest(req)
			span.Finish()
		}()
	}

	// Read the first three bundles.
	<-rt.reqc
	<-rt.reqc
	<-rt.reqc

	// Test that the fourth bundle isn't sent early.
	select {
	case <-rt.reqc:
		t.Errorf("bundle sent too early")
	case <-time.After(time.Second / 4):
		<-rt.reqc
	}

	// Test that there aren't extra bundles.
	select {
	case <-rt.reqc:
		t.Errorf("too many bundles sent")
	case <-time.After(time.Second):
	}
}

func TestWeights(t *testing.T) {
	const (
		expectedNumTraced   = 10100
		numTracedEpsilon    = 100
		expectedTotalWeight = 50000
		totalWeightEpsilon  = 5000
	)
	rng := rand.New(rand.NewSource(1))
	const delta = 2 * time.Millisecond
	for _, headerRate := range []float64{0.0, 0.5, 1.0} {
		// Simulate 10 seconds of requests arriving at 500qps.
		//
		// The sampling policy tries to sample 25% of them, but has a qps limit of
		// 100, so it will not be able to.  The returned weight should be higher
		// for some sampled requests to compensate.
		//
		// headerRate is the fraction of incoming requests that have a trace header
		// set.  The qps limit should not be exceeded, even if headerRate is high.
		sp, err := NewLimitedSampler(0.25, 100)
		if err != nil {
			t.Fatal(err)
		}
		s := sp.(*sampler)
		tm := time.Now()
		totalWeight := 0.0
		numTraced := 0
		seenLargeWeight := false
		for i := 0; i < 50000; i++ {
			d := s.sample(Parameters{HasTraceHeader: rng.Float64() < headerRate}, tm, rng.Float64())
			if d.Trace {
				numTraced++
			}
			if d.Sample {
				totalWeight += d.Weight
				if x := int(d.Weight) / 4; x <= 0 || x >= 100 || d.Weight != float64(x)*4.0 {
					t.Errorf("weight: got %f, want a small positive multiple of 4", d.Weight)
				}
				if d.Weight > 4 {
					seenLargeWeight = true
				}
			}
			tm = tm.Add(delta)
		}
		if !seenLargeWeight {
			t.Errorf("headerRate %f: never saw sample weight higher than 4.", headerRate)
		}
		if numTraced < expectedNumTraced-numTracedEpsilon || expectedNumTraced+numTracedEpsilon < numTraced {
			t.Errorf("headerRate %f: got %d traced requests, want ∈ [%d, %d]", headerRate, numTraced, expectedNumTraced-numTracedEpsilon, expectedNumTraced+numTracedEpsilon)
		}
		if totalWeight < expectedTotalWeight-totalWeightEpsilon || expectedTotalWeight+totalWeightEpsilon < totalWeight {
			t.Errorf("headerRate %f: got total weight %f want ∈ [%d, %d]", headerRate, totalWeight, expectedTotalWeight-totalWeightEpsilon, expectedTotalWeight+totalWeightEpsilon)
		}
	}
}

type alwaysTrace struct{}

func (a alwaysTrace) Sample(p Parameters) Decision {
	return Decision{Trace: true}
}

type neverTrace struct{}

func (a neverTrace) Sample(p Parameters) Decision {
	return Decision{Trace: false}
}

func TestPropagation(t *testing.T) {
	rt := newFakeRoundTripper()
	traceClient := newTestClient(rt)
	for _, header := range []string{
		`0123456789ABCDEF0123456789ABCDEF/42;o=0`,
		`0123456789ABCDEF0123456789ABCDEF/42;o=1`,
		`0123456789ABCDEF0123456789ABCDEF/42;o=2`,
		`0123456789ABCDEF0123456789ABCDEF/42;o=3`,
		`0123456789ABCDEF0123456789ABCDEF/0;o=0`,
		`0123456789ABCDEF0123456789ABCDEF/0;o=1`,
		`0123456789ABCDEF0123456789ABCDEF/0;o=2`,
		`0123456789ABCDEF0123456789ABCDEF/0;o=3`,
		``,
	} {
		for _, policy := range []SamplingPolicy{
			nil,
			alwaysTrace{},
			neverTrace{},
		} {
			traceClient.SetSamplingPolicy(policy)
			req, err := http.NewRequest("GET", "http://example.com/foo", nil)
			if err != nil {
				t.Fatal(err)
			}
			if header != "" {
				req.Header.Set("X-Cloud-Trace-Context", header)
			}

			span := traceClient.SpanFromRequest(req)

			req2, err := http.NewRequest("GET", "http://example.com/bar", nil)
			if err != nil {
				t.Fatal(err)
			}
			req3, err := http.NewRequest("GET", "http://example.com/baz", nil)
			if err != nil {
				t.Fatal(err)
			}
			span.NewRemoteChild(req2)
			span.NewRemoteChild(req3)

			var (
				t1, t2, t3 string
				s1, s2, s3 uint64
				o1, o2, o3 uint64
			)
			fmt.Sscanf(header, "%32s/%d;o=%d", &t1, &s1, &o1)
			fmt.Sscanf(req2.Header.Get("X-Cloud-Trace-Context"), "%32s/%d;o=%d", &t2, &s2, &o2)
			fmt.Sscanf(req3.Header.Get("X-Cloud-Trace-Context"), "%32s/%d;o=%d", &t3, &s3, &o3)

			if header == "" {
				if t2 != t3 {
					t.Errorf("expected the same trace ID in child requests, got %q %q", t2, t3)
				}
			} else {
				if t2 != t1 || t3 != t1 {
					t.Errorf("trace IDs should be passed to child requests")
				}
			}
			trace := policy == alwaysTrace{} || policy == nil && (o1&1) != 0
			if header == "" {
				if trace && (s2 == 0 || s3 == 0) {
					t.Errorf("got span IDs %d %d in child requests, want nonzero", s2, s3)
				}
				if trace && s2 == s3 {
					t.Errorf("got span IDs %d %d in child requests, should be different", s2, s3)
				}
				if !trace && (s2 != 0 || s3 != 0) {
					t.Errorf("got span IDs %d %d in child requests, want zero", s2, s3)
				}
			} else {
				if trace && (s2 == s1 || s3 == s1 || s2 == s3) {
					t.Errorf("parent span IDs in input and outputs should be all different, got %d %d %d", s1, s2, s3)
				}
				if !trace && (s2 != s1 || s3 != s1) {
					t.Errorf("parent span ID in input, %d, should have been equal to parent span IDs in output: %d %d", s1, s2, s3)
				}
			}
			expectTraceOption := policy == alwaysTrace{} || (o1&1) != 0
			if expectTraceOption != ((o2&1) != 0) || expectTraceOption != ((o3&1) != 0) {
				t.Errorf("tracing flag in child requests should be %t, got options %d %d", expectTraceOption, o2, o3)
			}
		}
	}
}
