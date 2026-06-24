/**
 * Prometheus metrics for the BFF. The registry is exposed at `/metrics` (only
 * when METRICS_ENABLED=true); default process metrics are collected under the
 * same flag. The exported histograms/counters are recorded from the request
 * logger, the proxy, and the state cache.
 */
import { Registry, collectDefaultMetrics, Histogram, Counter } from 'prom-client'

export const registry = new Registry()

export const httpRequestDuration = new Histogram({
  name: 'http_request_duration_seconds',
  help: 'Duration of incoming HTTP requests in seconds',
  labelNames: ['method', 'route', 'status_code'],
  buckets: [0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5],
  registers: [registry],
})

export const upstreamRequestDuration = new Histogram({
  name: 'upstream_request_duration_seconds',
  help: 'Duration of outgoing requests to conductor in seconds',
  labelNames: ['method', 'route'],
  buckets: [0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5],
  registers: [registry],
})

export const stateCacheHits = new Counter({
  name: 'state_cache_hits_total',
  help: 'Number of workspace state cache hits',
  registers: [registry],
})

export const stateCacheMisses = new Counter({
  name: 'state_cache_misses_total',
  help: 'Number of workspace state cache misses',
  registers: [registry],
})

if (process.env.METRICS_ENABLED === 'true') {
  collectDefaultMetrics({ register: registry })
}
