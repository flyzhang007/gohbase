// Copyright (C) 2015  The GoHBase Authors.  All rights reserved.
// This file is part of GoHBase.
// Use of this source code is governed by the Apache License 2.0
// that can be found in the COPYING file.

package region

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aristanetworks/goarista/test"
	"github.com/golang/mock/gomock"
	"github.com/tsuna/gohbase/hrpc"
	"github.com/tsuna/gohbase/test/mock"
	"golang.org/x/net/context"
)

func TestErrors(t *testing.T) {
	ue := UnrecoverableError{fmt.Errorf("oops")}
	if ue.Error() != "oops" {
		t.Errorf("Wrong error message. Got %q, wanted %q", ue, "oops")
	}
}

func TestWrite(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockConn := mock.NewMockReadWriteCloser(ctrl)
	c := &client{
		conn: mockConn,
	}

	// check if Write returns an error
	expectErr := errors.New("nope")
	mockConn.EXPECT().Write(gomock.Any()).Return(0, expectErr).Times(1)
	err := c.write([]byte("lol"))
	if diff := test.Diff(expectErr, err); diff != "" {
		t.Errorf("Expected: %#v\nReceived: %#v\nDiff:%s",
			expectErr, err, diff)
	}

	// check if it returns ErrShortWrite
	mockConn.EXPECT().Write(gomock.Any()).Return(1, nil).Times(1)
	err = c.write([]byte("lol"))
	if diff := test.Diff(ErrShortWrite, err); diff != "" {
		t.Errorf("Expected: %#v\nReceived: %#v\nDiff:%s",
			ErrShortWrite, err, diff)
	}

	// check if it actually writes the right data
	expected := []byte("lol")
	mockConn.EXPECT().Write(gomock.Any()).Return(3, nil).Times(1).Do(func(buf []byte) {
		if diff := test.Diff(expected, buf); diff != "" {
			t.Errorf("Expected: %#v\nReceived: %#v\nDiff:%s",
				expected, buf, diff)
		}
	})
	err = c.write(expected)
	if err != nil {
		t.Errorf("Was expecting error, but got one: %#v", err)
	}
}

func TestSendHello(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockConn := mock.NewMockReadWriteCloser(ctrl)
	c := &client{
		conn: mockConn,
	}

	// check if it's sending the right "hello" for RegionClient
	mockConn.EXPECT().Write(gomock.Any()).Return(35, nil).Times(1).Do(func(buf []byte) {
		expected := []byte("HBas\x00P\x00\x00\x00\x19\n\b\n\x06gopher\x12\rClientService")
		if diff := test.Diff(expected, buf); diff != "" {
			t.Errorf("Type RegionClient:\n Expected: %#v\nReceived: %#v\nDiff:%s",
				expected, buf, diff)
		}
	})
	err := c.sendHello(RegionClient)
	if err != nil {
		t.Errorf("Was expecting error, but got one: %#v", err)
	}

	// check if it sends the right "hello" for MasterClient
	mockConn.EXPECT().Write(gomock.Any()).Return(35, nil).Times(1).Do(func(buf []byte) {
		expected := []byte("HBas\x00P\x00\x00\x00\x19\n\b\n\x06gopher\x12\rMasterService")
		if diff := test.Diff(expected, buf); diff != "" {
			t.Errorf("Type MasterClient:\n Expected: %#v\nReceived: %#v\nDiff:%s",
				expected, buf, diff)
		}
	})
	err = c.sendHello(MasterClient)
	if err != nil {
		t.Errorf("Was expecting error, but got one: %#v", err)
	}
}

func TestFail(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockConn := mock.NewMockReadWriteCloser(ctrl)
	c := &client{
		conn: mockConn,
		done: make(chan struct{}),
		rpcs: make(chan hrpc.Call),
		sent: make(map[uint32]hrpc.Call),
	}
	expectedErr := errors.New("oooups")

	// check that connection Close is called only once
	mockConn.EXPECT().Close().Times(1)

	// run multiple in parallel to make sure everything is called only once
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			c.fail(expectedErr)
			wg.Done()
		}()
	}
	wg.Wait()

	// check if done channel is closed to notify goroutines to stop
	// if close(c.done) is called more than once, it would panic
	select {
	case <-time.After(2 * time.Second):
		t.Errorf("done hasn't been closed")
	case _, more := <-c.done:
		if more {
			t.Error("expected done to be closed")
		}
	}

	// check if rpcs channel is closed
	// if close(c.rpcs) is called more than once, it would panic
	select {
	case <-time.After(2 * time.Second):
		t.Errorf("rpcs hasn't been closed")
	case _, more := <-c.rpcs:
		if more {
			t.Error("expected rpcs to be closed")
		}
	}

	if diff := test.Diff(expectedErr, c.err); diff != "" {
		t.Errorf("Expected: %#v\nReceived: %#v\nDiff:%s",
			expectedErr, c.err, diff)
	}
}

type rpcMatcher struct {
	payload string
}

func (m rpcMatcher) Matches(x interface{}) bool {
	data, ok := x.([]byte)
	if !ok {
		return false
	}
	return strings.HasSuffix(string(data), m.payload)
}

func (m rpcMatcher) String() string {
	return "RPC payload is equal to " + m.payload
}

func newRPCMatcher(payload string) gomock.Matcher {
	return rpcMatcher{payload: payload}
}

func TestBufferedRPCsFail(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	queueSize := 100
	flushInterval := 1000 * time.Second
	mockConn := mock.NewMockReadWriteCloser(ctrl)
	mockConn.EXPECT().Close().Times(1)
	c := &client{
		conn:          mockConn,
		rpcs:          make(chan hrpc.Call, queueSize),
		done:          make(chan struct{}),
		sent:          make(map[uint32]hrpc.Call),
		rpcQueueSize:  queueSize,
		flushInterval: flushInterval,
	}

	// define rpcs behaviour
	var wgWrites sync.WaitGroup
	// we send less calls then queueSize so that sendBatch isn't triggered
	calls := make([]hrpc.Call, queueSize-1)
	for i := range calls {
		wgWrites.Add(1)
		mockCall := mock.NewMockCall(ctrl)
		mockCall.EXPECT().ResultChan().Return(make(chan hrpc.RPCResult, 1)).Times(1)
		calls[i] = mockCall
	}

	// queue calls
	for _, call := range calls {
		c.QueueRPC(call)
	}
	// process rpcs and close client in the middle of it to make sure that
	// all queued up rpcs are processed eventually
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		c.processRPCs()
		wg.Done()
	}()
	c.Close()
	wg.Wait()
	if len(c.rpcs) != 0 {
		t.Errorf("Expected all buffered rpcs to be processed, %d left", len(c.rpcs))
	}

	if len(c.sent) != 0 {
		t.Errorf("Expected all awaiting rpcs to be processed, %d left", len(c.sent))
	}
}

func TestQueueRPC(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	queueSize := 30
	flushInterval := 20 * time.Millisecond
	mockConn := mock.NewMockReadWriteCloser(ctrl)
	c := &client{
		conn:          mockConn,
		rpcs:          make(chan hrpc.Call, queueSize),
		done:          make(chan struct{}),
		sent:          make(map[uint32]hrpc.Call),
		rpcQueueSize:  queueSize,
		flushInterval: flushInterval,
	}
	var wgProcessRPCs sync.WaitGroup
	wgProcessRPCs.Add(1)
	go func() {
		c.processRPCs() // Writer goroutine
		wgProcessRPCs.Done()
	}()

	// define rpcs behaviour
	var wgWrites sync.WaitGroup
	calls := make([]hrpc.Call, 100)
	ctx := context.Background()
	for i := range calls {
		wgWrites.Add(1)
		mockCall := mock.NewMockCall(ctrl)
		mockCall.EXPECT().Name().Return("lol").Times(1)
		payload := fmt.Sprintf("rpc_%d", i)
		mockCall.EXPECT().Serialize().Return([]byte(payload), nil).Times(1)
		mockCall.EXPECT().Context().Return(ctx).Times(1)
		mockCall.EXPECT().ResultChan().Return(make(chan hrpc.RPCResult, 1)).Times(1)
		calls[i] = mockCall

		// we expect that it eventually writes to connection
		mockConn.EXPECT().Write(newRPCMatcher(payload)).Times(1).Return(15+len(payload), nil).Do(
			func(buf []byte) {
				wgWrites.Done()
			})
	}

	// queue calls in parallel
	for _, call := range calls {
		go func(call hrpc.Call) {
			c.QueueRPC(call)
		}(call)
	}

	// wait till all calls complete
	done := make(chan struct{})
	go func() {
		wgWrites.Wait()
		close(done)
	}()
	select {
	case <-time.After(2 * time.Second):
		t.Fatalf("rpcs hasn't been written")
	case <-done:
	}

	var wg sync.WaitGroup
	// now we fail the regionserver, and try to queue stuff
	mockConn.EXPECT().Close().Times(1)
	c.fail(errors.New("ooups"))
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := make(chan hrpc.RPCResult, 1)
			mockCall := mock.NewMockCall(ctrl)
			mockCall.EXPECT().ResultChan().Return(result).Times(1)
			c.QueueRPC(mockCall)
			r := <-result
			err, ok := r.Error.(UnrecoverableError)
			if !ok {
				t.Errorf("Expected UnrecoverableError error")
				return
			}
			if diff := test.Diff(ErrClientDead.error, err.error); diff != "" {
				t.Errorf("Expected: %s\nReceived: %s\nDiff:%s",
					ErrClientDead.error, err.error, diff)
			}
		}()
	}
	wg.Wait()
	wgProcessRPCs.Wait()
}

func TestUnrecoverableErrorWrite(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	queueSize := 1
	flushInterval := 10 * time.Millisecond
	mockConn := mock.NewMockReadWriteCloser(ctrl)
	c := &client{
		conn:          mockConn,
		rpcs:          make(chan hrpc.Call, queueSize),
		done:          make(chan struct{}),
		sent:          make(map[uint32]hrpc.Call),
		rpcQueueSize:  queueSize,
		flushInterval: flushInterval,
	}
	// define rpcs behaviour
	mockCall := mock.NewMockCall(ctrl)
	mockCall.EXPECT().Name().Return("lol").Times(1)
	payload := "rpc"
	mockCall.EXPECT().Serialize().Return([]byte(payload), nil).Times(1)
	mockCall.EXPECT().Context().Return(context.Background()).Times(1)
	result := make(chan hrpc.RPCResult, 1)
	mockCall.EXPECT().ResultChan().Return(result).Times(1)
	// we expect that it eventually writes to connection
	expErr := errors.New("Write failure")
	mockConn.EXPECT().Write(newRPCMatcher(payload)).Times(1).Return(0, expErr)
	mockConn.EXPECT().Close()

	c.QueueRPC(mockCall)
	c.processRPCs()
	r := <-result
	err, ok := r.Error.(UnrecoverableError)
	if !ok {
		t.Errorf("Expected UnrecoverableError error")
	}
	if diff := test.Diff(expErr, err.error); diff != "" {
		t.Errorf("Expected: %s\nReceived: %s\nDiff:%s",
			expErr, err.error, diff)
	}
	if len(c.sent) != 0 {
		t.Errorf("Expected all awaiting rpcs to be processed, %d left", len(c.sent))
	}
}

func TestUnrecoverableErrorRead(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	queueSize := 1
	flushInterval := 10 * time.Millisecond
	mockConn := mock.NewMockReadWriteCloser(ctrl)
	c := &client{
		conn:          mockConn,
		rpcs:          make(chan hrpc.Call, queueSize),
		done:          make(chan struct{}),
		sent:          make(map[uint32]hrpc.Call),
		rpcQueueSize:  queueSize,
		flushInterval: flushInterval,
	}
	// define rpcs behavior
	mockCall := mock.NewMockCall(ctrl)
	result := make(chan hrpc.RPCResult, 1)
	mockCall.EXPECT().ResultChan().Return(result).Times(1)
	mockConn.EXPECT().Read([]byte{0, 0, 0, 0}).Return(0, errors.New("read failure"))
	mockConn.EXPECT().Close()

	// pretend we already unqueued and sent the rpc
	c.sent[1] = mockCall
	// now try receiving result, should call fail
	c.receiveRPCs()
	_, more := <-c.done
	if more {
		t.Error("expected done to be closed")
	}
	// finish reading from c.rpc to clean up the c.sent map
	c.processRPCs()
	if len(c.sent) != 0 {
		t.Errorf("Expected all awaiting rpcs to be processed, %d left", len(c.sent))
	}
	r := <-result
	err, ok := r.Error.(UnrecoverableError)
	if !ok {
		t.Errorf("Expected UnrecoverableError error")
	}
	expErr := errors.New("failed to read: read failure")
	if diff := test.Diff(expErr, err.error); diff != "" {
		t.Errorf("Expected: %s\nReceived: %s\nDiff:%s",
			expErr, err.error, diff)
	}
}

func TestUnexpectedSendError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	queueSize := 1
	flushInterval := 10 * time.Millisecond
	mockConn := mock.NewMockReadWriteCloser(ctrl)
	c := &client{
		conn:          mockConn,
		rpcs:          make(chan hrpc.Call, queueSize),
		done:          make(chan struct{}),
		sent:          make(map[uint32]hrpc.Call),
		rpcQueueSize:  queueSize,
		flushInterval: flushInterval,
	}
	go c.processRPCs()
	// define rpcs behaviour
	mockCall := mock.NewMockCall(ctrl)
	mockCall.EXPECT().Name().Return("lol").Times(1)
	err := errors.New("Serialize error")
	mockCall.EXPECT().Serialize().Return(nil, err).Times(1)
	mockCall.EXPECT().Context().Return(context.Background()).Times(1)
	result := make(chan hrpc.RPCResult, 1)
	mockCall.EXPECT().ResultChan().Return(result).Times(1)

	c.QueueRPC(mockCall)
	r := <-result
	err = fmt.Errorf("failed to serialize RPC: %v", err)
	if diff := test.Diff(err, r.Error); diff != "" {
		t.Errorf("Expected: %s\nReceived: %s\nDiff:%s",
			err, r.Error, diff)
	}
	if len(c.sent) != 0 {
		t.Errorf("Expected all awaiting rpcs to be processed, %d left", len(c.sent))
	}
	// stop the go routine
	mockConn.EXPECT().Close()
	c.Close()
}

func TestSendBatch(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	queueSize := 1
	flushInterval := 10 * time.Millisecond
	mockConn := mock.NewMockReadWriteCloser(ctrl)
	c := &client{
		conn:          mockConn,
		rpcs:          make(chan hrpc.Call, queueSize),
		done:          make(chan struct{}),
		sent:          make(map[uint32]hrpc.Call),
		rpcQueueSize:  queueSize,
		flushInterval: flushInterval,
	}
	mockConn.EXPECT().Close()

	batch := make([]*call, 9)
	ctx := context.Background()
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	for i := range batch {
		mockCall := mock.NewMockCall(ctrl)
		if i < 3 {
			// we expect that these rpc are going to be ignored
			mockCall.EXPECT().Context().Return(canceledCtx).Times(1)
		} else if i < 6 {
			// we expect rpcs 3-5 to be written
			mockCall.EXPECT().Name().Return("lol").Times(1)
			payload := fmt.Sprintf("rpc_%d", i)
			mockCall.EXPECT().Serialize().Return([]byte(payload), nil).Times(1)
			mockCall.EXPECT().Context().Return(ctx).Times(1)
			// we expect that it eventually writes to connection
			i := i
			mockConn.EXPECT().Write(newRPCMatcher(payload)).Times(1).Return(
				15+len(payload), nil).Do(func(buf []byte) {
				if i == 5 {
					// we close on 6th rpc to check if sendBatch stop appropriately
					c.Close()
				}
			})
		} else if i == 6 {
			// last loop before return
			mockCall.EXPECT().Context().Return(ctx).Times(1)
		}
		// we expect the rest to be not even processed
		batch[i] = &call{id: uint32(i), Call: mockCall}
	}
	c.sendBatch(batch)
}

func TestFlushInterval(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	queueSize := 100000
	flushInterval := 30 * time.Millisecond
	mockConn := mock.NewMockReadWriteCloser(ctrl)
	c := &client{
		conn:          mockConn,
		rpcs:          make(chan hrpc.Call, queueSize),
		done:          make(chan struct{}),
		sent:          make(map[uint32]hrpc.Call),
		rpcQueueSize:  queueSize,
		flushInterval: flushInterval,
	}
	mockConn.EXPECT().Close()

	ctx := context.Background()
	var wgWrites sync.WaitGroup
	for i := 0; i < 100; i++ {
		wgWrites.Add(1)
		mockCall := mock.NewMockCall(ctrl)
		mockCall.EXPECT().Name().Return("lol").Times(1)
		payload := fmt.Sprintf("rpc_%d", i)
		mockCall.EXPECT().Serialize().Return([]byte(payload), nil).Times(1)
		mockCall.EXPECT().Context().Return(ctx).Times(1)
		mockCall.EXPECT().ResultChan().Return(make(chan hrpc.RPCResult, 1)).Times(1)
		mockConn.EXPECT().Write(newRPCMatcher(payload)).Times(1).Return(
			15+len(payload), nil).Do(func(buf []byte) {
			wgWrites.Done()
		})
		c.QueueRPC(mockCall)
	}
	var wgProcessRPCs sync.WaitGroup
	wgProcessRPCs.Add(1)
	go func() {
		c.processRPCs()
		wgProcessRPCs.Done()
	}()
	// test will timeout if some rpcs are never processed
	wgWrites.Wait()
	// clean up
	c.Close()
	wgProcessRPCs.Wait()
}

func TestQueueSize(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	queueSize := 10
	flushInterval := 10000 * time.Second
	mockConn := mock.NewMockReadWriteCloser(ctrl)
	c := &client{
		conn:          mockConn,
		rpcs:          make(chan hrpc.Call, queueSize),
		done:          make(chan struct{}),
		sent:          make(map[uint32]hrpc.Call),
		rpcQueueSize:  queueSize,
		flushInterval: flushInterval,
	}
	mockConn.EXPECT().Close()

	ctx := context.Background()
	var wgWrites sync.WaitGroup
	for i := 0; i < 10; i++ {
		wgWrites.Add(1)
		mockCall := mock.NewMockCall(ctrl)
		mockCall.EXPECT().Name().Return("lol").Times(1)
		payload := fmt.Sprintf("rpc_%d", i)
		mockCall.EXPECT().Serialize().Return([]byte(payload), nil).Times(1)
		mockCall.EXPECT().Context().Return(ctx).Times(1)
		mockCall.EXPECT().ResultChan().Return(make(chan hrpc.RPCResult, 1)).Times(1)
		mockConn.EXPECT().Write(newRPCMatcher(payload)).Times(1).Return(
			15+len(payload), nil).Do(func(buf []byte) {
			wgWrites.Done()
		})
		c.QueueRPC(mockCall)
	}
	var wgProcessRPCs sync.WaitGroup
	wgProcessRPCs.Add(1)
	go func() {
		c.processRPCs()
		wgProcessRPCs.Done()
	}()
	// test will timeout if some rpcs are never processed
	wgWrites.Wait()
	// clean up
	c.Close()
	wgProcessRPCs.Wait()
}
