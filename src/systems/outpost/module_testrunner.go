package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var jobGVR = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}

// testModule runs an integration-test suite as a Kubernetes Job inside the target
// cluster and blocks until it finishes, so an enqueue-and-wait CI step gates on the
// real result. A Job pod has ordinary cluster networking — it can reach the in-cluster
// control-plane services (conductor) that a forge sandbox is deliberately walled off
// from — which is why post-deploy verification runs here and not in a sandbox. The
// control plane still holds no cluster credentials: it enqueues a command, and this,
// running in the target cluster, creates and reaps the Job.
type testModule struct {
	client    dynamic.Interface
	defaultNS string
}

func newTestModule(client dynamic.Interface) *testModule {
	return &testModule{
		client:    client,
		defaultNS: envOrDefault("TEST_NAMESPACE", envOrDefault("POD_NAMESPACE", "default")),
	}
}

func (m *testModule) Name() string { return "test" }

// Start has no watches — the test module is purely command-driven.
func (m *testModule) Start(_ context.Context, _ func(Event)) error { return nil }

// HandleCommand runs one integration-test Job to completion. Command type "run"
// payload:
//
//	image             (required) the test-runner image, e.g. a pytest suite
//	command           (optional) container command override ([]string)
//	env               (optional) extra env for the runner (map[string]string)
//	namespace         (optional) defaults to the module namespace
//	timeout_secs      (optional) hard deadline; default 1200
//	image_pull_secret (optional) defaults to "forgejo-pull"
//
// It returns normally when the Job completes (tests passed) and errors when the Job
// fails (tests failed or the deadline elapsed) — the error becomes the command's
// failed status so the polling CI step fails the run.
func (m *testModule) HandleCommand(ctx context.Context, c Command) ([]Event, error) {
	if c.Type != "run" {
		return nil, fmt.Errorf("test: unknown command type %q", c.Type)
	}
	image := payloadString(c.Payload, "image")
	if image == "" {
		return nil, fmt.Errorf("test: run requires image")
	}
	ns := m.defaultNS
	if v := payloadString(c.Payload, "namespace"); v != "" {
		ns = v
	}
	timeoutSecs := payloadInt(c.Payload, "timeout_secs", 1200)
	pullSecret := payloadString(c.Payload, "image_pull_secret")
	if pullSecret == "" {
		pullSecret = "forgejo-pull"
	}

	container := map[string]any{"name": "test", "image": image}
	if cmd := payloadStringSlice(c.Payload, "command"); len(cmd) > 0 {
		container["command"] = toAnySlice(cmd)
	}
	if env := payloadStringMap(c.Payload, "env"); len(env) > 0 {
		envList := make([]any, 0, len(env))
		for k, v := range env {
			envList = append(envList, map[string]any{"name": k, "value": v})
		}
		container["env"] = envList
	}

	job := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"generateName": "outpost-test-",
			"namespace":    ns,
			"labels":       map[string]any{"app.kubernetes.io/managed-by": "outpost-test"},
		},
		"spec": map[string]any{
			// backoffLimit 0: a test failure is a result, not a flake to retry.
			"backoffLimit":            int64(0),
			"activeDeadlineSeconds":   int64(timeoutSecs),
			"ttlSecondsAfterFinished": int64(600),
			"template": map[string]any{
				"spec": map[string]any{
					"restartPolicy":    "Never",
					"imagePullSecrets": []any{map[string]any{"name": pullSecret}},
					"containers":       []any{container},
				},
			},
		},
	}}

	created, err := m.client.Resource(jobGVR).Namespace(ns).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("test: create job in %s: %w", ns, err)
	}
	jobName := created.GetName()
	slog.Info("test: job created", "namespace", ns, "job", jobName, "image", image)

	// Best-effort reap on the way out — ttlSecondsAfterFinished is the backstop, but
	// deleting eagerly keeps the namespace clean between runs.
	defer func() {
		policy := metav1.DeletePropagationBackground
		_ = m.client.Resource(jobGVR).Namespace(ns).Delete(context.Background(), jobName, metav1.DeleteOptions{PropagationPolicy: &policy}) //nolint:errcheck
	}()

	deadline := time.Now().Add(time.Duration(timeoutSecs+60) * time.Second)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("test: job %s/%s did not finish within %ds", ns, jobName, timeoutSecs)
		}
		got, err := m.client.Resource(jobGVR).Namespace(ns).Get(ctx, jobName, metav1.GetOptions{})
		if err != nil {
			continue // transient — keep polling
		}
		complete, failed, reason := jobOutcome(got.Object)
		if complete {
			slog.Info("test: job passed", "namespace", ns, "job", jobName)
			return []Event{{
				Integration: "test",
				Type:        "test-passed",
				Payload:     map[string]any{"job": jobName, "namespace": ns, "image": image},
			}}, nil
		}
		if failed {
			return nil, fmt.Errorf("test: integration tests failed (job %s/%s): %s", ns, jobName, reason)
		}
	}
}

// jobOutcome reads a Job's terminal condition from its unstructured status. Returns
// (complete, failed, reason); both false while still running.
func jobOutcome(obj map[string]any) (complete, failed bool, reason string) {
	conds, found, err := unstructured.NestedSlice(obj, "status", "conditions")
	if err != nil || !found {
		return false, false, ""
	}
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		ctype, _ := cm["type"].(string)
		cstatus, _ := cm["status"].(string)
		if cstatus != "True" {
			continue
		}
		switch ctype {
		case "Complete":
			return true, false, ""
		case "Failed":
			msg, _ := cm["message"].(string)
			r, _ := cm["reason"].(string)
			return false, true, trimJoin(r, msg)
		}
	}
	return false, false, ""
}

func trimJoin(a, b string) string {
	switch {
	case a != "" && b != "":
		return a + ": " + b
	case a != "":
		return a
	default:
		return b
	}
}

func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
