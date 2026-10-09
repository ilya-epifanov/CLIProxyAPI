package usagemetrics

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// AuthLister returns the current credentials, e.g. coreauth.Manager.List.
type AuthLister func() []*coreauth.Auth

// StoreStats reports the usage store's losses.
type StoreStats interface {
	DroppedRecords() uint64
	WriteErrors() uint64
}

var (
	requestsDesc = prometheus.NewDesc("cliproxyapi_upstream_requests_total",
		"Upstream request attempts by provider, credential, model and HTTP status (200 on success).",
		[]string{"provider", "auth_index", "model", "code"}, nil)
	tokensDesc = prometheus.NewDesc("cliproxyapi_upstream_tokens_total",
		"Upstream tokens by provider, credential, model and type. Types do not overlap: input excludes cached input and output excludes reasoning.",
		[]string{"provider", "auth_index", "model", "type"}, nil)
	authInfoDesc = prometheus.NewDesc("cliproxyapi_auth_info",
		"Names for an auth_index; always 1.",
		[]string{"auth_index", "provider", "label", "email"}, nil)
	droppedDesc = prometheus.NewDesc("cliproxyapi_usage_store_dropped_records_total",
		"Usage records the store discarded because its writer fell behind or it was closed.", nil, nil)
	writeErrorsDesc = prometheus.NewDesc("cliproxyapi_usage_store_write_errors_total",
		"Usage records lost because a store write failed.", nil, nil)
)

type collector struct {
	aggregator *Aggregator
	auths      AuthLister
	store      StoreStats
}

// NewHandler serves the usage counters, credential names, store losses and
// the Go runtime and process metrics. auths and store may be nil.
func NewHandler(aggregator *Aggregator, auths AuthLister, store StoreStats) http.Handler {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		&collector{aggregator: aggregator, auths: auths, store: store},
	)
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- requestsDesc
	ch <- tokensDesc
	ch <- authInfoDesc
	if c.store != nil {
		ch <- droppedDesc
		ch <- writeErrorsDesc
	}
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	for _, s := range c.aggregator.snapshot() {
		for code, n := range s.requestsByCode {
			ch <- prometheus.MustNewConstMetric(requestsDesc, prometheus.CounterValue, float64(n),
				s.key.provider, s.key.authIndex, s.key.model, strconv.Itoa(code))
		}
		for i, n := range s.tokens {
			ch <- prometheus.MustNewConstMetric(tokensDesc, prometheus.CounterValue, float64(n),
				s.key.provider, s.key.authIndex, s.key.model, tokenTypeLabels[i])
		}
	}
	if c.auths != nil {
		// A repeated label set would fail the whole scrape, so each index is reported once.
		seen := map[string]bool{}
		for _, auth := range c.auths() {
			index := strings.TrimSpace(auth.EnsureIndex())
			if index == "" || seen[index] {
				continue
			}
			seen[index] = true
			ch <- prometheus.MustNewConstMetric(authInfoDesc, prometheus.GaugeValue, 1,
				index, labelValue(auth.Provider), labelValue(auth.Label), labelValue(authEmail(auth)))
		}
	}
	if c.store != nil {
		ch <- prometheus.MustNewConstMetric(droppedDesc, prometheus.CounterValue, float64(c.store.DroppedRecords()))
		ch <- prometheus.MustNewConstMetric(writeErrorsDesc, prometheus.CounterValue, float64(c.store.WriteErrors()))
	}
}

// labelValue trims s and replaces invalid UTF-8, which MustNewConstMetric
// rejects by panicking inside the scrape. Auth labels come from credential files.
func labelValue(s string) string {
	return strings.TrimSpace(strings.ToValidUTF8(s, "\uFFFD"))
}

// authEmail returns the account email of an OAuth credential. Auth.AccountInfo
// is not used because it returns the key itself for API-key credentials.
func authEmail(auth *coreauth.Auth) string {
	if email, ok := auth.Metadata["email"].(string); ok && strings.TrimSpace(email) != "" {
		return email
	}
	return auth.Attributes["email"]
}
