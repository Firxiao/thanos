// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

package e2e_test

import (
	"context"
	"log"
	"net/http"
	"net/http/httputil"
	"testing"
	"time"

	"github.com/efficientgo/e2e"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/relabel"

	"github.com/thanos-io/thanos/pkg/promclient"
	"github.com/thanos-io/thanos/pkg/receive"
	"github.com/thanos-io/thanos/pkg/testutil"
	"github.com/thanos-io/thanos/test/e2e/e2ethanos"
)

type DebugTransport struct{}

func (DebugTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	_, err := httputil.DumpRequestOut(r, false)
	if err != nil {
		return nil, err
	}
	return http.DefaultTransport.RoundTrip(r)
}

func ErrorHandler(_ http.ResponseWriter, _ *http.Request, err error) {
	log.Print("Response from receiver")
	log.Print(err)
}

func TestReceive(t *testing.T) {
	t.Parallel()

	t.Run("single_ingestor", func(t *testing.T) {
		/*
			The single_ingestor suite represents the simplest possible configuration of Thanos Receive.
			 ┌──────────┐
			 │  Prom    │
			 └────┬─────┘
			      │
			 ┌────▼─────┐
			 │ Ingestor │
			 └────┬─────┘
			      │
			 ┌────▼─────┐
			 │  Query   │
			 └──────────┘
			NB: Made with asciiflow.com - you can copy & paste the above there to modify.
		*/

		t.Parallel()
		e, err := e2e.NewDockerEnvironment("e2e_receive_single_ingestor")
		testutil.Ok(t, err)
		t.Cleanup(e2ethanos.CleanScenario(t, e))

		// Setup Router Ingestor.
		i := e2ethanos.NewReceiveBuilder(e, "ingestor").WithIngestionEnabled().Init()
		testutil.Ok(t, e2e.StartAndWaitReady(i))

		// Setup Prometheus
		prom := e2ethanos.NewPrometheus(e, "1", e2ethanos.DefaultPromConfig("prom1", 0, e2ethanos.RemoteWriteEndpoint(i.InternalEndpoint("remote-write")), "", e2ethanos.LocalPrometheusTarget), "", e2ethanos.DefaultPrometheusImage())
		testutil.Ok(t, e2e.StartAndWaitReady(prom))

		q := e2ethanos.NewQuerierBuilder(e, "1", i.InternalEndpoint("grpc")).Init()
		testutil.Ok(t, e2e.StartAndWaitReady(q))

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		t.Cleanup(cancel)

		testutil.Ok(t, q.WaitSumMetricsWithOptions(e2e.Equals(1), []string{"thanos_store_nodes_grpc_connections"}, e2e.WaitMissingMetrics()))

		// We expect the data from each Prometheus instance to be replicated twice across our ingesting instances
		queryAndAssertSeries(t, ctx, q.Endpoint("http"), e2ethanos.QueryUpWithoutInstance, time.Now, promclient.QueryOptions{
			Deduplicate: false,
		}, []model.Metric{
			{
				"job":        "myself",
				"prometheus": "prom1",
				"receive":    "receive-ingestor",
				"replica":    "0",
				"tenant_id":  "default-tenant",
			},
		})
	})

	t.Run("router_replication", func(t *testing.T) {
		/*
			The router_replication suite configures separate routing and ingesting components.
			It verifies that data ingested from Prometheus instances through the router is successfully replicated twice
			across the ingestors.

			  ┌───────┐       ┌───────┐      ┌───────┐
			  │       │       │       │      │       │
			  │ Prom1 │       │ Prom2 │      │ Prom3 │
			  │       │       │       │      │       │
			  └───┬───┘       └───┬───┘      └──┬────┘
			      │           ┌───▼────┐        │
			      └───────────►        ◄────────┘
			                  │ Router │
			      ┌───────────┤        ├──────────┐
			      │           └───┬────┘          │
			┌─────▼─────┐   ┌─────▼─────┐   ┌─────▼─────┐
			│           │   │           │   │           │
			│ Ingestor1 │   │ Ingestor2 │   │ Ingestor3 │
			│           │   │           │   │           │
			└─────┬─────┘   └─────┬─────┘   └─────┬─────┘
			      │           ┌───▼───┐           │
			      │           │       │           │
			      └───────────► Query ◄───────────┘
			                  │       │
			                  └───────┘

			NB: Made with asciiflow.com - you can copy & paste the above there to modify.
		*/

		t.Parallel()
		e, err := e2e.NewDockerEnvironment("e2e_receive_router_replication")
		testutil.Ok(t, err)
		t.Cleanup(e2ethanos.CleanScenario(t, e))

		// Setup 3 ingestors.
		i1 := e2ethanos.NewReceiveBuilder(e, "i1").WithIngestionEnabled().Init()
		i2 := e2ethanos.NewReceiveBuilder(e, "i2").WithIngestionEnabled().Init()
		i3 := e2ethanos.NewReceiveBuilder(e, "i3").WithIngestionEnabled().Init()

		h := receive.HashringConfig{
			Endpoints: []string{
				i1.InternalEndpoint("grpc"),
				i2.InternalEndpoint("grpc"),
				i3.InternalEndpoint("grpc"),
			},
		}

		// Setup 1 distributor with double replication
		r1 := e2ethanos.NewReceiveBuilder(e, "r1").WithRouting(2, h).Init()
		testutil.Ok(t, e2e.StartAndWaitReady(i1, i2, i3, r1))

		prom1 := e2ethanos.NewPrometheus(e, "1", e2ethanos.DefaultPromConfig("prom1", 0, e2ethanos.RemoteWriteEndpoint(r1.InternalEndpoint("remote-write")), "", e2ethanos.LocalPrometheusTarget), "", e2ethanos.DefaultPrometheusImage())
		prom2 := e2ethanos.NewPrometheus(e, "2", e2ethanos.DefaultPromConfig("prom2", 0, e2ethanos.RemoteWriteEndpoint(r1.InternalEndpoint("remote-write")), "", e2ethanos.LocalPrometheusTarget), "", e2ethanos.DefaultPrometheusImage())
		prom3 := e2ethanos.NewPrometheus(e, "3", e2ethanos.DefaultPromConfig("prom3", 0, e2ethanos.RemoteWriteEndpoint(r1.InternalEndpoint("remote-write")), "", e2ethanos.LocalPrometheusTarget), "", e2ethanos.DefaultPrometheusImage())
		testutil.Ok(t, e2e.StartAndWaitReady(prom1, prom2, prom3))

		q := e2ethanos.NewQuerierBuilder(e, "1", i1.InternalEndpoint("grpc"), i2.InternalEndpoint("grpc"), i3.InternalEndpoint("grpc")).Init()
		testutil.Ok(t, e2e.StartAndWaitReady(q))

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		t.Cleanup(cancel)

		testutil.Ok(t, q.WaitSumMetricsWithOptions(e2e.Equals(3), []string{"thanos_store_nodes_grpc_connections"}, e2e.WaitMissingMetrics()))

		expectedReplicationFactor := 2.0

		queryAndAssert(t, ctx, q.Endpoint("http"), func() string { return "count(up) by (prometheus)" }, time.Now, promclient.QueryOptions{
			Deduplicate: false,
		}, model.Vector{
			&model.Sample{
				Metric: model.Metric{
					"prometheus": "prom1",
				},
				Value: model.SampleValue(expectedReplicationFactor),
			},
			&model.Sample{
				Metric: model.Metric{
					"prometheus": "prom2",
				},
				Value: model.SampleValue(expectedReplicationFactor),
			},
			&model.Sample{
				Metric: model.Metric{
					"prometheus": "prom3",
				},
				Value: model.SampleValue(expectedReplicationFactor),
			},
		})
	})

	t.Run("routing_tree", func(t *testing.T) {
		/*
			The routing_tree suite configures a valid and plausible, but non-trivial topology of receiver components.
			Crucially, the first router routes to both a routing component, and a receiving component. This demonstrates
			Receiver's ability to handle arbitrary depth receiving trees.

			Router1 is configured to duplicate data twice, once to Ingestor1, and once to Router2,
			Router2 is also configured to duplicate data twice, once to Ingestor2, and once to Ingestor3.

			           ┌───────┐         ┌───────┐
			           │       │         │       │
			           │ Prom1 ├──┐   ┌──┤ Prom2 │
			           │       │  │   │  │       │
			           └───────┘  │   │  └───────┘
			                   ┌──▼───▼──┐
			                   │         │
			                   │ Router1 │
			              ┌────┤         ├───────┐
			              │    └─────────┘       │
			          ┌───▼─────┐          ┌─────▼─────┐
			          │         │          │           │
			          │ Router2 │          │ Ingestor1 │
			      ┌───┤         ├───┐      │           │
			      │   └─────────┘   │      └─────┬─────┘
			┌─────▼─────┐      ┌────▼──────┐     │
			│           │      │           │     │
			│ Ingestor2 │      │ Ingestor3 │     │
			│           │      │           │     │
			└─────┬─────┘      └─────┬─────┘     │
			      │             ┌────▼────┐      │
			      │             │         │      │
			      └─────────────►  Query  ◄──────┘
			                    │         │
			                    └─────────┘

			NB: Made with asciiflow.com - you can copy & paste the above there to modify.
		*/

		t.Parallel()
		e, err := e2e.NewDockerEnvironment("e2e_receive_routing_tree")
		testutil.Ok(t, err)
		t.Cleanup(e2ethanos.CleanScenario(t, e))

		// Setup ingestors.
		i1 := e2ethanos.NewReceiveBuilder(e, "i1").WithIngestionEnabled().Init()
		i2 := e2ethanos.NewReceiveBuilder(e, "i2").WithIngestionEnabled().Init()
		i3 := e2ethanos.NewReceiveBuilder(e, "i3").WithIngestionEnabled().Init()

		// Setup distributors
		r2 := e2ethanos.NewReceiveBuilder(e, "r2").WithRouting(2, receive.HashringConfig{
			Endpoints: []string{
				i2.InternalEndpoint("grpc"),
				i3.InternalEndpoint("grpc"),
			},
		}).Init()
		r1 := e2ethanos.NewReceiveBuilder(e, "r1").WithRouting(2, receive.HashringConfig{
			Endpoints: []string{
				i1.InternalEndpoint("grpc"),
				r2.InternalEndpoint("grpc"),
			},
		}).Init()
		testutil.Ok(t, e2e.StartAndWaitReady(i1, i2, i3, r1, r2))

		// Setup Prometheus.
		prom1 := e2ethanos.NewPrometheus(e, "1", e2ethanos.DefaultPromConfig("prom1", 0, e2ethanos.RemoteWriteEndpoint(r1.InternalEndpoint("remote-write")), "", e2ethanos.LocalPrometheusTarget), "", e2ethanos.DefaultPrometheusImage())
		prom2 := e2ethanos.NewPrometheus(e, "2", e2ethanos.DefaultPromConfig("prom2", 0, e2ethanos.RemoteWriteEndpoint(r1.InternalEndpoint("remote-write")), "", e2ethanos.LocalPrometheusTarget), "", e2ethanos.DefaultPrometheusImage())
		testutil.Ok(t, e2e.StartAndWaitReady(prom1, prom2))

		//Setup Querier
		q := e2ethanos.NewQuerierBuilder(e, "1", i1.InternalEndpoint("grpc"), i2.InternalEndpoint("grpc"), i3.InternalEndpoint("grpc")).Init()
		testutil.Ok(t, e2e.StartAndWaitReady(q))

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		t.Cleanup(cancel)

		testutil.Ok(t, q.WaitSumMetricsWithOptions(e2e.Equals(3), []string{"thanos_store_nodes_grpc_connections"}, e2e.WaitMissingMetrics()))

		expectedReplicationFactor := 3.0

		queryAndAssert(t, ctx, q.Endpoint("http"), func() string { return "count(up) by (prometheus)" }, time.Now, promclient.QueryOptions{
			Deduplicate: false,
		}, model.Vector{
			&model.Sample{
				Metric: model.Metric{
					"prometheus": "prom1",
				},
				Value: model.SampleValue(expectedReplicationFactor),
			},
			&model.Sample{
				Metric: model.Metric{
					"prometheus": "prom2",
				},
				Value: model.SampleValue(expectedReplicationFactor),
			},
		})
	})

	t.Run("hashring", func(t *testing.T) {
		/*
			The hashring suite creates three receivers, each with a Prometheus
			remote-writing data to it. However, due to the hashing of the labels,
			the time series from the Prometheus is forwarded to a different
			receiver in the hashring than the one handling the request.
			The querier queries all the receivers and the test verifies
			the time series are forwarded to the correct receive node.

			                      ┌───────┐
			                      │       │
			                      │ Prom2 │
			                      │       │
			                      └───┬───┘
			                          │
			                          │
			    ┌────────┐      ┌─────▼─────┐     ┌───────┐
			    │        │      │           │     │       │
			    │ Prom1  │      │ Router    │     │ Prom3 │
			    │        │      │ Ingestor2 │     │       │
			    └───┬────┘      │           │     └───┬───┘
			        │           └──▲──┬──▲──┘         │
			        │              │  │  │            │
			   ┌────▼──────┐       │  │  │       ┌────▼──────┐
			   │           ◄───────┘  │  └───────►           │
			   │ Router    │          │          │ Router    │
			   │ Ingestor1 ◄──────────┼──────────► Ingestor3 │
			   │           │          │          │           │
			   └─────┬─────┘          │          └────┬──────┘
			         │                │               │
			         │            ┌───▼───┐           │
			         │            │       │           │
			         └────────────► Query ◄───────────┘
			                      │       │
			                      └───────┘
		*/
		t.Parallel()

		e, err := e2e.NewDockerEnvironment("e2e_test_receive_hashring")
		testutil.Ok(t, err)
		t.Cleanup(e2ethanos.CleanScenario(t, e))

		r1 := e2ethanos.NewReceiveBuilder(e, "1").WithIngestionEnabled()
		r2 := e2ethanos.NewReceiveBuilder(e, "2").WithIngestionEnabled()
		r3 := e2ethanos.NewReceiveBuilder(e, "3").WithIngestionEnabled()

		h := receive.HashringConfig{
			Endpoints: []string{
				r1.InternalEndpoint("grpc"),
				r2.InternalEndpoint("grpc"),
				r3.InternalEndpoint("grpc"),
			},
		}

		// Create with hashring config watcher.
		r1Runnable := r1.WithRouting(1, h).Init()
		r2Runnable := r2.WithRouting(1, h).Init()
		r3Runnable := r3.WithRouting(1, h).Init()
		testutil.Ok(t, e2e.StartAndWaitReady(r1Runnable, r2Runnable, r3Runnable))

		prom1 := e2ethanos.NewPrometheus(e, "1", e2ethanos.DefaultPromConfig("prom1", 0, e2ethanos.RemoteWriteEndpoint(r1.InternalEndpoint("remote-write")), "", e2ethanos.LocalPrometheusTarget), "", e2ethanos.DefaultPrometheusImage())
		prom2 := e2ethanos.NewPrometheus(e, "2", e2ethanos.DefaultPromConfig("prom2", 0, e2ethanos.RemoteWriteEndpoint(r2.InternalEndpoint("remote-write")), "", e2ethanos.LocalPrometheusTarget), "", e2ethanos.DefaultPrometheusImage())
		prom3 := e2ethanos.NewPrometheus(e, "3", e2ethanos.DefaultPromConfig("prom3", 0, e2ethanos.RemoteWriteEndpoint(r3.InternalEndpoint("remote-write")), "", e2ethanos.LocalPrometheusTarget), "", e2ethanos.DefaultPrometheusImage())
		testutil.Ok(t, err)
		testutil.Ok(t, e2e.StartAndWaitReady(prom1, prom2, prom3))

		q := e2ethanos.NewQuerierBuilder(e, "1", r1.InternalEndpoint("grpc"), r2.InternalEndpoint("grpc"), r3.InternalEndpoint("grpc")).Init()
		testutil.Ok(t, err)
		testutil.Ok(t, e2e.StartAndWaitReady(q))

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		t.Cleanup(cancel)

		testutil.Ok(t, q.WaitSumMetricsWithOptions(e2e.Equals(3), []string{"thanos_store_nodes_grpc_connections"}, e2e.WaitMissingMetrics()))

		queryAndAssertSeries(t, ctx, q.Endpoint("http"), e2ethanos.QueryUpWithoutInstance, time.Now, promclient.QueryOptions{
			Deduplicate: false,
		}, []model.Metric{
			{
				"job":        "myself",
				"prometheus": "prom1",
				"receive":    "receive-2",
				"replica":    "0",
				"tenant_id":  "default-tenant",
			},
			{
				"job":        "myself",
				"prometheus": "prom2",
				"receive":    "receive-1",
				"replica":    "0",
				"tenant_id":  "default-tenant",
			},
			{
				"job":        "myself",
				"prometheus": "prom3",
				"receive":    "receive-2",
				"replica":    "0",
				"tenant_id":  "default-tenant",
			},
		})
	})

	t.Run("replication", func(t *testing.T) {
		t.Parallel()

		e, err := e2e.NewDockerEnvironment("e2e_test_receive_replication")
		testutil.Ok(t, err)
		t.Cleanup(e2ethanos.CleanScenario(t, e))

		// The replication suite creates three receivers but only one
		// receives Prometheus remote-written data. The querier queries all
		// receivers and the test verifies that the time series are
		// replicated to all of the nodes.

		r1 := e2ethanos.NewReceiveBuilder(e, "1").WithIngestionEnabled()
		r2 := e2ethanos.NewReceiveBuilder(e, "2").WithIngestionEnabled()
		r3 := e2ethanos.NewReceiveBuilder(e, "3").WithIngestionEnabled()

		h := receive.HashringConfig{
			Endpoints: []string{
				r1.InternalEndpoint("grpc"),
				r2.InternalEndpoint("grpc"),
				r3.InternalEndpoint("grpc"),
			},
		}

		// Create with hashring config.
		r1Runnable := r1.WithRouting(3, h).Init()
		r2Runnable := r2.WithRouting(3, h).Init()
		r3Runnable := r3.WithRouting(3, h).Init()
		testutil.Ok(t, e2e.StartAndWaitReady(r1Runnable, r2Runnable, r3Runnable))

		prom1 := e2ethanos.NewPrometheus(e, "1", e2ethanos.DefaultPromConfig("prom1", 0, e2ethanos.RemoteWriteEndpoint(r1.InternalEndpoint("remote-write")), "", e2ethanos.LocalPrometheusTarget), "", e2ethanos.DefaultPrometheusImage())
		testutil.Ok(t, e2e.StartAndWaitReady(prom1))

		q := e2ethanos.NewQuerierBuilder(e, "1", r1.InternalEndpoint("grpc"), r2.InternalEndpoint("grpc"), r3.InternalEndpoint("grpc")).Init()
		testutil.Ok(t, e2e.StartAndWaitReady(q))

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		t.Cleanup(cancel)

		testutil.Ok(t, q.WaitSumMetricsWithOptions(e2e.Equals(3), []string{"thanos_store_nodes_grpc_connections"}, e2e.WaitMissingMetrics()))

		queryAndAssertSeries(t, ctx, q.Endpoint("http"), e2ethanos.QueryUpWithoutInstance, time.Now, promclient.QueryOptions{
			Deduplicate: false,
		}, []model.Metric{
			{
				"job":        "myself",
				"prometheus": "prom1",
				"receive":    "receive-1",
				"replica":    "0",
				"tenant_id":  "default-tenant",
			},
			{
				"job":        "myself",
				"prometheus": "prom1",
				"receive":    "receive-2",
				"replica":    "0",
				"tenant_id":  "default-tenant",
			},
			{
				"job":        "myself",
				"prometheus": "prom1",
				"receive":    "receive-3",
				"replica":    "0",
				"tenant_id":  "default-tenant",
			},
		})
	})

	t.Run("replication_with_outage", func(t *testing.T) {
		t.Parallel()

		e, err := e2e.NewDockerEnvironment("e2e_test_receive_replication_with_outage")
		testutil.Ok(t, err)
		t.Cleanup(e2ethanos.CleanScenario(t, e))

		// The replication suite creates a three-node hashring but one of the
		// receivers is dead. In this case, replication should still
		// succeed and the time series should be replicated to the other nodes.

		r1 := e2ethanos.NewReceiveBuilder(e, "1").WithIngestionEnabled()
		r2 := e2ethanos.NewReceiveBuilder(e, "2").WithIngestionEnabled()
		r3 := e2ethanos.NewReceiveBuilder(e, "3").WithIngestionEnabled()

		h := receive.HashringConfig{
			Endpoints: []string{
				r1.InternalEndpoint("grpc"),
				r2.InternalEndpoint("grpc"),
				r3.InternalEndpoint("grpc"),
			},
		}

		// Create with hashring config.
		r1Runnable := r1.WithRouting(3, h).Init()
		r2Runnable := r2.WithRouting(3, h).Init()
		testutil.Ok(t, e2e.StartAndWaitReady(r1Runnable, r2Runnable))

		prom1 := e2ethanos.NewPrometheus(e, "1", e2ethanos.DefaultPromConfig("prom1", 0, e2ethanos.RemoteWriteEndpoint(r1.InternalEndpoint("remote-write")), "", e2ethanos.LocalPrometheusTarget), "", e2ethanos.DefaultPrometheusImage())
		testutil.Ok(t, e2e.StartAndWaitReady(prom1))

		q := e2ethanos.NewQuerierBuilder(e, "1", r1.InternalEndpoint("grpc"), r2.InternalEndpoint("grpc")).Init()
		testutil.Ok(t, e2e.StartAndWaitReady(q))

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		t.Cleanup(cancel)

		testutil.Ok(t, q.WaitSumMetricsWithOptions(e2e.Equals(2), []string{"thanos_store_nodes_grpc_connections"}, e2e.WaitMissingMetrics()))

		queryAndAssertSeries(t, ctx, q.Endpoint("http"), e2ethanos.QueryUpWithoutInstance, time.Now, promclient.QueryOptions{
			Deduplicate: false,
		}, []model.Metric{
			{
				"job":        "myself",
				"prometheus": "prom1",
				"receive":    "receive-1",
				"replica":    "0",
				"tenant_id":  "default-tenant",
			},
			{
				"job":        "myself",
				"prometheus": "prom1",
				"receive":    "receive-2",
				"replica":    "0",
				"tenant_id":  "default-tenant",
			},
		})
	})

	t.Run("multitenancy", func(t *testing.T) {
		t.Parallel()

		e, err := e2e.NewDockerEnvironment("e2e_test_for_multitenancy")
		testutil.Ok(t, err)
		t.Cleanup(e2ethanos.CleanScenario(t, e))

		r1 := e2ethanos.NewReceiveBuilder(e, "1").WithIngestionEnabled()

		h := receive.HashringConfig{
			Endpoints: []string{
				r1.InternalEndpoint("grpc"),
			},
		}

		// Create with hashring config.
		r1Runnable := r1.WithRouting(1, h).Init()
		testutil.Ok(t, e2e.StartAndWaitReady(r1Runnable))

		rp1 := e2ethanos.NewReverseProxy(e, "1", "tenant-1", "http://"+r1.InternalEndpoint("remote-write"))
		rp2 := e2ethanos.NewReverseProxy(e, "2", "tenant-2", "http://"+r1.InternalEndpoint("remote-write"))
		testutil.Ok(t, e2e.StartAndWaitReady(rp1, rp2))

		prom1 := e2ethanos.NewPrometheus(e, "1", e2ethanos.DefaultPromConfig("prom1", 0, "http://"+rp1.InternalEndpoint("http")+"/api/v1/receive", "", e2ethanos.LocalPrometheusTarget), "", e2ethanos.DefaultPrometheusImage())
		prom2 := e2ethanos.NewPrometheus(e, "2", e2ethanos.DefaultPromConfig("prom2", 0, "http://"+rp2.InternalEndpoint("http")+"/api/v1/receive", "", e2ethanos.LocalPrometheusTarget), "", e2ethanos.DefaultPrometheusImage())
		testutil.Ok(t, e2e.StartAndWaitReady(prom1, prom2))

		q := e2ethanos.NewQuerierBuilder(e, "1", r1.InternalEndpoint("grpc")).Init()
		testutil.Ok(t, e2e.StartAndWaitReady(q))
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		t.Cleanup(cancel)

		testutil.Ok(t, q.WaitSumMetricsWithOptions(e2e.Equals(1), []string{"thanos_store_nodes_grpc_connections"}, e2e.WaitMissingMetrics()))
		queryAndAssertSeries(t, ctx, q.Endpoint("http"), e2ethanos.QueryUpWithoutInstance, time.Now, promclient.QueryOptions{
			Deduplicate: false,
		}, []model.Metric{
			{
				"job":        "myself",
				"prometheus": "prom1",
				"receive":    "receive-1",
				"replica":    "0",
				"tenant_id":  "tenant-1",
			},
			{
				"job":        "myself",
				"prometheus": "prom2",
				"receive":    "receive-1",
				"replica":    "0",
				"tenant_id":  "tenant-2",
			},
		})
	})

	t.Run("relabel", func(t *testing.T) {
		t.Parallel()
		e, err := e2e.NewDockerEnvironment("e2e_receive_relabel")
		testutil.Ok(t, err)
		t.Cleanup(e2ethanos.CleanScenario(t, e))

		// Setup Router Ingestor.
		i := e2ethanos.NewReceiveBuilder(e, "ingestor").
			WithIngestionEnabled().
			WithRelabelConfigs([]*relabel.Config{
				{
					Action: relabel.LabelDrop,
					Regex:  relabel.MustNewRegexp("prometheus"),
				},
			}).Init()

		testutil.Ok(t, e2e.StartAndWaitReady(i))

		// Setup Prometheus
		prom := e2ethanos.NewPrometheus(e, "1", e2ethanos.DefaultPromConfig("prom1", 0, e2ethanos.RemoteWriteEndpoint(i.InternalEndpoint("remote-write")), "", e2ethanos.LocalPrometheusTarget), "", e2ethanos.DefaultPrometheusImage())
		testutil.Ok(t, e2e.StartAndWaitReady(prom))

		q := e2ethanos.NewQuerierBuilder(e, "1", i.InternalEndpoint("grpc")).Init()
		testutil.Ok(t, e2e.StartAndWaitReady(q))

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		t.Cleanup(cancel)

		testutil.Ok(t, q.WaitSumMetricsWithOptions(e2e.Equals(1), []string{"thanos_store_nodes_grpc_connections"}, e2e.WaitMissingMetrics()))
		// Label `prometheus` should be dropped.
		queryAndAssertSeries(t, ctx, q.Endpoint("http"), e2ethanos.QueryUpWithoutInstance, time.Now, promclient.QueryOptions{
			Deduplicate: false,
		}, []model.Metric{
			{
				"job":       "myself",
				"receive":   "receive-ingestor",
				"replica":   "0",
				"tenant_id": "default-tenant",
			},
		})
	})
}
