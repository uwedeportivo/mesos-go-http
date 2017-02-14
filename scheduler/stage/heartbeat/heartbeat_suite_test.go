package heartbeat_test

import (
	. "github.com/ondrej-smola/mesos-go-http/scheduler/stage/heartbeat"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"context"
	"github.com/ondrej-smola/mesos-go-http/flow"
	"github.com/ondrej-smola/mesos-go-http/scheduler"
	"testing"
	"time"
)

func TestAck(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Heartbeat stage suite")
}

var _ = Describe("FrameworkId stage", func() {

	It("Set deadling from subscribed event", func(done Done) {
		fwId := New(WithMaxMissedHeartbeats(1))
		sink := flow.NewTestFlow()
		fwId.Via(sink)

		go func() {
			defer GinkgoRecover()
			id := "5"
			pull := sink.ExpectPull()
			s := scheduler.Subscribed(id)
			s.Subscribed.HeartbeatIntervalSeconds = 0.005 // 5 millis
			pull.Message(s)

			// this pull times out
			pull = sink.ExpectPull()
			start := time.Now()
			<-pull.Ctx.Done()
			Expect(time.Now().Sub(start)).To(BeNumerically("~", 10*time.Millisecond, time.Millisecond))
			pull.Error(pull.Ctx.Err())

			// this pull does not
			pull = sink.ExpectPull()
			time.Sleep(5 * time.Millisecond)
			pull.Message(&scheduler.Event{})
		}()

		_, err := fwId.Pull(context.Background())
		Expect(err).To(Succeed())

		_, err = fwId.Pull(context.Background())
		Expect(err).To(Equal(context.DeadlineExceeded))

		_, err = fwId.Pull(context.Background())
		Expect(err).To(Succeed())

		close(done)
	})

	It("Set deadling from configuration", func(done Done) {
		fwId := New(WithMaxMissedHeartbeats(3), WithHeartbeatDeadline(10*time.Millisecond))
		sink := flow.NewTestFlow()
		fwId.Via(sink)

		go func() {
			defer GinkgoRecover()
			id := "5"
			pull := sink.ExpectPull()
			s := scheduler.Subscribed(id)
			s.Subscribed.HeartbeatIntervalSeconds = 0.005 // 5 millis - should be IGNORED
			pull.Message(s)

			// this pull times out
			pull = sink.ExpectPull()
			start := time.Now()
			<-pull.Ctx.Done()
			Expect(time.Now().Sub(start)).To(BeNumerically("~", 40*time.Millisecond, time.Millisecond))
			pull.Error(pull.Ctx.Err())

			// this pull does not
			pull = sink.ExpectPull()
			time.Sleep(30 * time.Millisecond)
			pull.Message(&scheduler.Event{})
		}()

		_, err := fwId.Pull(context.Background())
		Expect(err).To(Succeed())

		_, err = fwId.Pull(context.Background())
		Expect(err).To(Equal(context.DeadlineExceeded))

		_, err = fwId.Pull(context.Background())
		Expect(err).To(Succeed())

		close(done)
	})
})