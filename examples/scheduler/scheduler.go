package main

import (
	"context"
	"github.com/go-kit/kit/log"
	"github.com/ondrej-smola/mesos-go-http"
	"github.com/ondrej-smola/mesos-go-http/backoff"
	"github.com/ondrej-smola/mesos-go-http/client"
	"github.com/ondrej-smola/mesos-go-http/client/leader"
	"github.com/ondrej-smola/mesos-go-http/examples/scheduler/metrics"
	"github.com/ondrej-smola/mesos-go-http/flow"
	"github.com/ondrej-smola/mesos-go-http/scheduler"
	"github.com/ondrej-smola/mesos-go-http/scheduler/stage/ack"
	"github.com/ondrej-smola/mesos-go-http/scheduler/stage/callopt"
	"github.com/ondrej-smola/mesos-go-http/scheduler/stage/fwid"
	"github.com/ondrej-smola/mesos-go-http/scheduler/stage/heartbeat"
	"github.com/ondrej-smola/mesos-go-http/scheduler/stage/monitor"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

type app struct {
	cfg           *config
	tasksLaunched int
	wants         mesos.Resources
	log           log.Logger
}

func run(cfg *config) {
	// Setup
	w := log.NewSyncWriter(os.Stderr)
	logger := log.NewContext(log.NewLogfmtLogger(w)).With("ts", log.DefaultTimestampUTC)

	metricsBackend := metrics.New()

	sched := scheduler.Blueprint(
		leader.New(
			cfg.endpoints,
			leader.WithLogger(log.NewContext(logger).With("src", "leader_client")),
			leader.WithClientOpts(client.WithRecordIOFraming()),
		),
	)

	blueprint := flow.BlueprintBuilder().
		Append(monitor.Blueprint(metricsBackend)).
		Append(callopt.Blueprint(scheduler.Filters(mesos.RefuseSecondsWithJitter(3*time.Second)))).
		Append(heartbeat.Blueprint()).
		Append(ack.Blueprint()).
		Append(fwid.Blueprint()).
		RunWith(sched, flow.WithLogger(log.NewContext(logger).With("src", "flow")))
	//

	go serveMetrics(metricsBackend, cfg.metricsBind, logger)

	wants := mesos.Resources{
		mesos.BuildResource().Name("cpus").Scalar(cfg.taskCpus).Build(),
		mesos.BuildResource().Name("mem").Scalar(cfg.taskMem).Build(),
	}

	a := &app{
		cfg:   cfg,
		wants: wants,
		log:   log.NewContext(logger).With("src", "main"),
	}

	ctx := context.Background()

	retry := backoff.New(backoff.Always()).New(ctx)
	defer retry.Close()

	var msg flow.Message

	for attempt := range retry.Attempts() {
		a.tasksLaunched = 0
		a.log.Log("event", "connecting", "attempt", attempt)
		fl := blueprint.Mat()
		err := fl.Push(scheduler.Subscribe(mesos.FrameworkInfo{User: "root", Name: "test"}), ctx)
		for err == nil {
			msg, err = fl.Pull(ctx)
			if err == nil {
				switch m := msg.(type) {
				case *scheduler.Event:
					a.log.Log("event", "message_received", "type", m.Type.String())
					switch m.Type {
					case scheduler.Event_SUBSCRIBED:
						retry.Reset()
					case scheduler.Event_UPDATE:
						status := m.Update.Status
						a.log.Log(
							"event", "status_update",
							"task_id", status.TaskID.Value,
							"status", status.State.String(),
							"msg", status.Message,
						)
					case scheduler.Event_OFFERS:
						err = a.handleOffers(m.Offers.Offers, fl)
					}
				}
			}
		}

		a.log.Log("event", "failed", "attempt", attempt, "err", err)
		fl.Close()
	}
}
func serveMetrics(metrics *metrics.PrometheusMetrics, endpoint string, log log.Logger) {
	if endpoint == "" {
		log.Log("event", "metrics_server_disable")
		return
	}

	promRegistry := prometheus.NewRegistry()
	metrics.MustRegister(promRegistry)

	l, err := net.Listen("tcp", endpoint)
	if err != nil {
		log.Log("event", "http_listener", "err", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(promRegistry, promhttp.HandlerOpts{}))

	srv := http.Server{
		Addr:    endpoint,
		Handler: mux,
	}

	log.Log("event", "metrics_server", "listening", l.Addr().String())
	if err := srv.Serve(l); err != nil {
		log.Log("event", "metrics_server", "err", err)
		os.Exit(1)
	}
}

func (a *app) handleOffers(offers []mesos.Offer, flow flow.Flow) error {
	useShell := false

	for _, o := range offers {
		logger := log.NewContext(a.log).With("offer", o.ID.Value)

		offerResources := mesos.Resources(o.Resources)
		logger.Log("resources", offerResources)
		tasks := []mesos.TaskInfo{}

		for a.tasksLaunched < a.cfg.numTasks && offerResources.ContainsAll(a.wants) {
			a.tasksLaunched++
			taskID := a.tasksLaunched

			t := mesos.TaskInfo{
				Name:    "Task " + strconv.Itoa(taskID),
				TaskID:  mesos.TaskID{Value: strconv.Itoa(taskID)},
				AgentID: o.AgentID,
				Container: &mesos.ContainerInfo{
					Type: mesos.ContainerInfo_DOCKER.Enum(),
					Docker: &mesos.ContainerInfo_DockerInfo{
						Image:   a.cfg.taskImage,
						Network: mesos.ContainerInfo_DockerInfo_NONE.Enum(),
					},
				},
				Command: &mesos.CommandInfo{
					Shell:     &useShell,
					Value:     nilIfEmptyString(a.cfg.taskCmd),
					Arguments: a.cfg.taskArgs,
				},
				Resources: offerResources.Find(a.wants),
			}

			tasks = append(tasks, t)
			offerResources = offerResources.Subtract(t.Resources...)
		}

		if len(tasks) > 0 {
			logger.Log("event", "launch", "count", len(tasks))
			accept := scheduler.Accept(
				scheduler.OfferOperations{scheduler.OpLaunch(tasks...)}.WithOffers(o.ID),
			)
			err := flow.Push(accept, context.Background())
			if err != nil {
				return err
			}
		} else {
			logger.Log("event", "declined")
			err := flow.Push(scheduler.Decline(o.ID), context.Background())
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func nilIfEmptyString(cmd string) *string {
	if cmd == "" {
		return nil
	} else {
		return &cmd
	}
}
