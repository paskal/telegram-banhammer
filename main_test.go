package main

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMain_Simple(t *testing.T) {
	//var webhookSent int32
	//ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	//	atomic.StoreInt32(&webhookSent, 1)
	//	assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
	//
	//	b, e := io.ReadAll(r.Body)
	//	defer r.Body.Close()
	//
	//	assert.Nil(t, e)
	//	assert.Equal(t, "Comment: env test", string(b))
	//}))
	//defer ts.Close()

	os.Args = []string{"test", "--apphash=123", "--appid=321", "--channel-id=456", "--phone=+1234567890"}

	done := make(chan struct{})
	go func() {
		<-done
		e := syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		require.NoError(t, e)
	}()

	finished := make(chan struct{})
	// TODO add mocks for call
	tgClient = &tgClientInterfaceMock{}
	tgAPI = &tgAPIInterfaceMock{}
	go func() {
		main()
		time.Sleep(time.Second)
		//assert.Eventually(t, func() bool {
		//	return atomic.LoadInt32(&webhookSent) == int32(1)
		//}, time.Second, 100*time.Millisecond, "webhook was not sent")
		close(finished)
	}()

	// defer cleanup because require check below can fail
	defer func() {
		close(done)
		<-finished
	}()
	//	test something
}
