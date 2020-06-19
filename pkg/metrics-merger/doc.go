/*
Copyright 2020 Intel Coporation.

SPDX-License-Identifier: Apache-2.0
*/

// Package metricsmerger contains an HTTP handler that scrapes
// multiple different metrics endpoints and exposes those under a
// single endpoint.  This way, the metrics data of multiple containers
// can be exposed under a single endpoint, as expected by Prometheus
// (see
// https://github.com/helm/charts/tree/master/stable/prometheus#scraping-pod-metrics-via-annotations).
//
// This code was originally copied from https://github.com/rebuy-de/exporter-merger and
// modified to run inside an existing container.
package metricsmerger
