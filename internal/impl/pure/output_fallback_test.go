package pure

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/benthosdev/benthos/v4/internal/bundle"
	"github.com/benthosdev/benthos/v4/internal/component/output"
	"github.com/benthosdev/benthos/v4/internal/component/processor"
	"github.com/benthosdev/benthos/v4/internal/manager/mock"
	"github.com/benthosdev/benthos/v4/internal/message"
)

var _ output.Streamed = &fallbackBroker{}

func TestFallbackOutputBasic(t *testing.T) {
	dir := t.TempDir()

	outOne, outTwo, outThree := output.NewConfig(), output.NewConfig(), output.NewConfig()
	outOne.Type, outTwo.Type, outThree.Type = "http_client", "file", "file"
	outOne.HTTPClient.URL = "http://localhost:11111111/badurl"
	outOne.HTTPClient.NumRetries = 1
	outOne.HTTPClient.Retry = "1ms"
	outTwo.File.Path = filepath.Join(dir, "two", `bar-${!count("fallbacktofoo")}-${!count("fallbacktobar")}.txt`)
	outTwo.File.Codec = "all-bytes"
	outThree.File.Path = "/dev/null"

	procOne, procTwo, procThree := processor.NewConfig(), processor.NewConfig(), processor.NewConfig()
	procOne.Type, procTwo.Type, procThree.Type = "bloblang", "bloblang", "bloblang"
	procOne.Bloblang = `root = "this-should-never-appear %v".format(count("fallbacktofoo")) + content()`
	procTwo.Bloblang = `root = "two-" + content()`
	procThree.Bloblang = `root = "this-should-never-appear %v".format(count("fallbacktobar")) + content()`

	outOne.Processors = append(outOne.Processors, procOne)
	outTwo.Processors = append(outTwo.Processors, procTwo)
	outThree.Processors = append(outThree.Processors, procThree)

	conf := output.NewConfig()
	conf.Type = "fallback"
	conf.Fallback = append(conf.Fallback, outOne, outTwo, outThree)

	s, err := bundle.AllOutputs.Init(conf, mock.NewManager())
	require.NoError(t, err)

	sendChan := make(chan message.Transaction)
	resChan := make(chan error)
	require.NoError(t, s.Consume(sendChan))

	t.Cleanup(func() {
		s.CloseAsync()
		require.NoError(t, s.WaitForClose(time.Second))
	})

	inputs := []string{
		"first", "second", "third", "fourth",
	}
	expFiles := map[string]string{
		"./two/bar-2-1.txt": "two-first",
		"./two/bar-4-2.txt": "two-second",
		"./two/bar-6-3.txt": "two-third",
		"./two/bar-8-4.txt": "two-fourth",
	}

	for _, input := range inputs {
		testMsg := message.QuickBatch([][]byte{[]byte(input)})
		select {
		case sendChan <- message.NewTransaction(testMsg, resChan):
		case <-time.After(time.Second * 2):
			t.Fatal("Action timed out")
		}

		select {
		case res := <-resChan:
			if res != nil {
				t.Fatal(res)
			}
		case <-time.After(time.Second * 2):
			t.Fatal("Action timed out")
		}
	}

	for k, exp := range expFiles {
		k = filepath.Join(dir, k)
		fileBytes, err := os.ReadFile(k)
		if err != nil {
			t.Errorf("Expected file '%v' could not be read: %v", k, err)
			continue
		}
		if act := string(fileBytes); exp != act {
			t.Errorf("Wrong contents for file '%v': %v != %v", k, act, exp)
		}
	}
}

func TestFallbackDoubleClose(t *testing.T) {
	oTM, err := newFallbackBroker([]output.Streamed{&mock.OutputChanneled{}})
	if err != nil {
		t.Fatal(err)
	}

	// This shouldn't cause a panic
	oTM.CloseAsync()
	oTM.CloseAsync()
}

//------------------------------------------------------------------------------

func TestFallbackHappyPath(t *testing.T) {
	tCtx, done := context.WithTimeout(context.Background(), time.Second*5)
	defer done()

	outputs := []output.Streamed{}
	mockOutputs := []*mock.OutputChanneled{
		{},
		{},
		{},
	}

	for _, o := range mockOutputs {
		outputs = append(outputs, o)
	}

	readChan := make(chan message.Transaction)
	resChan := make(chan error)

	oTM, err := newFallbackBroker(outputs)
	if err != nil {
		t.Error(err)
		return
	}
	if err = oTM.Consume(readChan); err != nil {
		t.Error(err)
		return
	}

	for i := 0; i < 10; i++ {
		content := [][]byte{[]byte(fmt.Sprintf("hello world %v", i))}
		select {
		case readChan <- message.NewTransaction(message.QuickBatch(content), resChan):
		case <-time.After(time.Second):
			t.Errorf("Timed out waiting for broker send")
			return
		}

		go func() {
			var ts message.Transaction
			select {
			case ts = <-mockOutputs[0].TChan:
				if !bytes.Equal(ts.Payload.Get(0).Get(), content[0]) {
					t.Errorf("Wrong content returned %s != %s", ts.Payload.Get(0).Get(), content[0])
				}
			case <-mockOutputs[1].TChan:
				t.Error("Received message in wrong order")
				return
			case <-mockOutputs[2].TChan:
				t.Error("Received message in wrong order")
				return
			case <-time.After(time.Second):
				t.Errorf("Timed out waiting for broker propagate")
				return
			}
			require.NoError(t, ts.Ack(tCtx, nil))
		}()

		select {
		case res := <-resChan:
			if res != nil {
				t.Error(res)
			}
		case <-time.After(time.Second):
			t.Errorf("Timed out responding to broker")
			return
		}
	}

	oTM.CloseAsync()
	if err := oTM.WaitForClose(time.Second * 10); err != nil {
		t.Error(err)
	}
}

func TestFallbackHappyishPath(t *testing.T) {
	tCtx, done := context.WithTimeout(context.Background(), time.Second*5)
	defer done()

	outputs := []output.Streamed{}
	mockOutputs := []*mock.OutputChanneled{
		{},
		{},
		{},
	}

	for _, o := range mockOutputs {
		outputs = append(outputs, o)
	}

	readChan := make(chan message.Transaction)
	resChan := make(chan error)

	oTM, err := newFallbackBroker(outputs)
	if err != nil {
		t.Error(err)
		return
	}
	if err = oTM.Consume(readChan); err != nil {
		t.Error(err)
		return
	}

	for i := 0; i < 10; i++ {
		content := [][]byte{[]byte(fmt.Sprintf("hello world %v", i))}
		select {
		case readChan <- message.NewTransaction(message.QuickBatch(content), resChan):
		case <-time.After(time.Second):
			t.Errorf("Timed out waiting for broker send")
			return
		}

		go func() {
			var ts message.Transaction
			select {
			case ts = <-mockOutputs[0].TChan:
				if !bytes.Equal(ts.Payload.Get(0).Get(), content[0]) {
					t.Errorf("Wrong content returned %s != %s", ts.Payload.Get(0).Get(), content[0])
				}
			case <-mockOutputs[1].TChan:
				t.Error("Received message in wrong order")
				return
			case <-mockOutputs[2].TChan:
				t.Error("Received message in wrong order")
				return
			case <-time.After(time.Second):
				t.Errorf("Timed out waiting for broker propagate")
				return
			}
			go func() {
				require.NoError(t, ts.Ack(tCtx, errors.New("test err")))
			}()

			select {
			case ts = <-mockOutputs[1].TChan:
				if !bytes.Equal(ts.Payload.Get(0).Get(), content[0]) {
					t.Errorf("Wrong content returned %s != %s", ts.Payload.Get(0).Get(), content[0])
				}
			case <-mockOutputs[0].TChan:
				t.Error("Received message in wrong order")
				return
			case <-mockOutputs[2].TChan:
				t.Error("Received message in wrong order")
				return
			case <-time.After(time.Second):
				t.Errorf("Timed out waiting for broker propagate")
				return
			}
			require.NoError(t, ts.Ack(tCtx, nil))
		}()

		select {
		case res := <-resChan:
			if res != nil {
				t.Error(res)
			}
		case <-time.After(time.Second):
			t.Errorf("Timed out responding to broker")
			return
		}
	}

	close(readChan)
	if err := oTM.WaitForClose(time.Second * 10); err != nil {
		t.Error(err)
	}
}

func TestFallbackAllFail(t *testing.T) {
	tCtx, done := context.WithTimeout(context.Background(), time.Second*5)
	defer done()

	outputs := []output.Streamed{}
	mockOutputs := []*mock.OutputChanneled{
		{},
		{},
		{},
	}

	for _, o := range mockOutputs {
		outputs = append(outputs, o)
	}

	readChan := make(chan message.Transaction)
	resChan := make(chan error)

	oTM, err := newFallbackBroker(outputs)
	if err != nil {
		t.Fatal(err)
	}
	if err = oTM.Consume(readChan); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 10; i++ {
		content := [][]byte{[]byte(fmt.Sprintf("hello world %v", i))}
		select {
		case readChan <- message.NewTransaction(message.QuickBatch(content), resChan):
		case <-time.After(time.Second):
			t.Fatalf("Timed out waiting for broker send")
		}

		testErr := errors.New("test error")
		go func() {
			for j := 0; j < 3; j++ {
				var ts message.Transaction
				select {
				case ts = <-mockOutputs[j%3].TChan:
					if !bytes.Equal(ts.Payload.Get(0).Get(), content[0]) {
						t.Errorf("Wrong content returned %s != %s", ts.Payload.Get(0).Get(), content[0])
					}
				case <-mockOutputs[(j+1)%3].TChan:
					t.Errorf("Received message in wrong order: %v != %v", j%3, (j+1)%3)
					return
				case <-mockOutputs[(j+2)%3].TChan:
					t.Errorf("Received message in wrong order: %v != %v", j%3, (j+2)%3)
					return
				case <-time.After(time.Second):
					t.Errorf("Timed out waiting for broker propagate")
					return
				}
				go func() {
					require.NoError(t, ts.Ack(tCtx, testErr))
				}()
			}
		}()

		select {
		case res := <-resChan:
			if exp, act := testErr, res; exp != act {
				t.Errorf("Wrong error returned: %v != %v", act, exp)
			}
		case <-time.After(time.Second):
			t.Fatal("Timed out responding to broker")
		}
	}

	oTM.CloseAsync()
	if err := oTM.WaitForClose(time.Second * 10); err != nil {
		t.Error(err)
	}
}

func TestFallbackAllFailParallel(t *testing.T) {
	tCtx, done := context.WithTimeout(context.Background(), time.Second*5)
	defer done()

	outputs := []output.Streamed{}
	mockOutputs := []*mock.OutputChanneled{
		{},
		{},
		{},
	}

	for _, o := range mockOutputs {
		outputs = append(outputs, o)
	}

	readChan := make(chan message.Transaction)

	oTM, err := newFallbackBroker(outputs)
	if err != nil {
		t.Fatal(err)
	}
	if err = oTM.Consume(readChan); err != nil {
		t.Fatal(err)
	}

	resChans := make([]chan error, 10)
	for i := range resChans {
		resChans[i] = make(chan error, 1)
	}

	tallies := [3]int32{}

	wg := sync.WaitGroup{}
	wg.Add(len(mockOutputs))
	testErr := errors.New("test error")

	for i, o := range mockOutputs {
		i := i
		o := o
		go func() {
			defer wg.Done()
			for range resChans {
				select {
				case ts := <-o.TChan:
					go require.NoError(t, ts.Ack(tCtx, testErr))
				case <-time.After(time.Second):
					t.Errorf("Timed out waiting for broker propagate")
					return
				}
				atomic.AddInt32(&tallies[i], 1)
			}
		}()
	}

	for i, resChan := range resChans {
		select {
		case readChan <- message.NewTransaction(message.QuickBatch([][]byte{[]byte("foo")}), resChan):
		case <-time.After(time.Second):
			t.Fatal("Timed out waiting for broker send", strconv.Itoa(i))
		}
	}

	for _, resChan := range resChans {
		select {
		case res := <-resChan:
			if exp, act := testErr, res; exp != act {
				t.Errorf("Wrong error returned: %v != %v", act, exp)
			}
		case <-time.After(time.Second):
			t.Error("Timed out responding to broker")
		}
	}

	wg.Wait()
	for _, tally := range tallies {
		if int(tally) != len(resChans) {
			t.Errorf("Wrong count of propagated messages: %v", tally)
		}
	}

	close(readChan)
	if err := oTM.WaitForClose(time.Second * 10); err != nil {
		t.Error(err)
	}
}

//------------------------------------------------------------------------------
