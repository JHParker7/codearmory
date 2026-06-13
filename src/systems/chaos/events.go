package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// internalEvent is the envelope the outpost-gateway dispatcher delivers. The
// gateway routes by integration; this service only ever receives chaos events.
type internalEvent struct {
	EventID     string         `json:"event_id"`
	OutpostID   string         `json:"outpost_id"`
	OrgID       string         `json:"org_id"`
	Integration string         `json:"integration"`
	Type        string         `json:"type"`
	Payload     map[string]any `json:"payload"`
}

// handleInternalEvent consumes events the outpost chaos module emitted (relayed
// by the gateway dispatcher). Authentication is the shared-key HMAC over
// "event:{integration}:{type}:{event_id}". Events are idempotent: the dispatcher
// is at-least-once, so duplicate verdicts for a terminal experiment are no-ops.
func handleInternalEvent(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("chaos").Start(r.Context(), "handleInternalEvent")
	defer span.End()

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	// Authenticate over the raw bytes before parsing — don't let an unauthenticated
	// caller probe JSON validity or make us parse pre-auth.
	if !verifyInternal("event", body,
		r.Header.Get("X-Internal-Token"), r.Header.Get("X-Internal-Timestamp")) {
		span.SetStatus(codes.Error, "invalid internal token")
		slog.WarnContext(ctx, "internal event: invalid token")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var ev internalEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	// Defense-in-depth: the gateway dispatcher routes by integration, but the
	// shared internal key also authenticates other consumers — never act on an
	// event for a different integration.
	if ev.Integration != "" && ev.Integration != "chaos" {
		slog.WarnContext(ctx, "internal event: ignoring non-chaos integration", "integration", ev.Integration)
		w.WriteHeader(http.StatusOK)
		return
	}

	experimentID, _ := ev.Payload["experiment_id"].(string)
	span.SetAttributes(
		attribute.String("event.type", ev.Type),
		attribute.String("experiment.id", experimentID),
	)
	if experimentID == "" {
		// Nothing to correlate; ack so the dispatcher does not retry forever.
		w.WriteHeader(http.StatusOK)
		return
	}

	switch ev.Type {
	case "run-started":
		if exp, changed, err := markRunning(ctx, experimentID); err != nil {
			if isNotFound(err) {
				w.WriteHeader(http.StatusOK)
				return
			}
			span.RecordError(err)
			http.Error(w, "failed to update experiment", http.StatusInternalServerError)
			return
		} else if changed {
			slog.InfoContext(ctx, "experiment running", "experiment_id", exp.ExperimentID)
		}
	case "verdict":
		verdict, _ := ev.Payload["verdict"].(string)
		phase, _ := ev.Payload["phase"].(string)
		failStep, _ := ev.Payload["fail_step"].(string)
		probe := stringifyProbe(ev.Payload["probe_success_percentage"])
		exp, changed, err := applyVerdict(ctx, experimentID, verdict, phase, failStep, probe)
		if err != nil {
			if isNotFound(err) {
				w.WriteHeader(http.StatusOK)
				return
			}
			span.RecordError(err)
			http.Error(w, "failed to apply verdict", http.StatusInternalServerError)
			return
		}
		if changed {
			slog.InfoContext(ctx, "experiment verdict applied", "experiment_id", exp.ExperimentID, "status", exp.Status, "verdict", exp.Verdict)
			// Only count and notify on a real terminal transition — a verdict event
			// can also land while the run is still in flight (status stays running).
			if isTerminal(exp.Status) && exp.Status != StatusStopped {
				meterExperimentsResolved.Add(ctx, 1, attrStatus(exp.Status))
				if exp.Status == StatusPass {
					notifyHooks(ctx, eventExperimentPassed, exp)
				} else {
					notifyHooks(ctx, eventExperimentFailed, exp)
				}
			}
		}
	default:
		slog.DebugContext(ctx, "internal event: ignoring unknown type", "type", ev.Type)
	}

	span.SetStatus(codes.Ok, "")
	w.WriteHeader(http.StatusOK)
}

func stringifyProbe(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		// JSON numbers decode as float64; render without trailing zeros.
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return ""
	}
}
