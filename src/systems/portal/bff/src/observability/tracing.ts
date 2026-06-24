/**
 * OpenTelemetry tracing bootstrap for the BFF. Imported for its side effect
 * first thing in index.ts: when OTEL_EXPORTER_OTLP_ENDPOINT is set it wires up
 * an OTLP exporter, otherwise tracing is inert. Exports a no-op-safe `tracer`
 * the proxy and state routes start spans on.
 */
import { trace } from '@opentelemetry/api'
import { NodeTracerProvider } from '@opentelemetry/sdk-trace-node'
import { BatchSpanProcessor } from '@opentelemetry/sdk-trace-base'
import { OTLPTraceExporter } from '@opentelemetry/exporter-trace-otlp-http'
import { Resource } from '@opentelemetry/resources'

if (process.env.OTEL_EXPORTER_OTLP_ENDPOINT) {
  // SDK reads OTEL_EXPORTER_OTLP_ENDPOINT from env automatically.
  const provider = new NodeTracerProvider({
    resource: new Resource({ 'service.name': 'portal-bff' }),
  })
  provider.addSpanProcessor(new BatchSpanProcessor(new OTLPTraceExporter()))
  provider.register()
  process.on('SIGTERM', () => provider.shutdown().finally(() => process.exit(0)))
}

export const tracer = trace.getTracer('portal-bff')
