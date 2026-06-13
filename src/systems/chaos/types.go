package main

import "time"

// Experiment lifecycle states. The terminal states Pass/Fail/Error are written
// verbatim from the chaos module's verdict events and MUST match the
// success_states/failure_states declared for the chaos/run-experiment action in
// registry-manifest.json. status_contract_test.go asserts this invariant.
const (
	StatusPending = "pending"
	StatusRunning = "running"
	StatusPass    = "Pass"
	StatusFail    = "Fail"
	StatusError   = "Error"
	StatusStopped = "stopped"
)

// runExperimentSuccessStates / runExperimentFailureStates mirror the async
// action contract published in the registry manifest for chaos/run-experiment.
var runExperimentSuccessStates = []string{StatusPass}
var runExperimentFailureStates = []string{StatusFail, StatusError}

// Built-in experiment types. These map to litmuschaos ChaosExperiment names that
// the outpost chaos module knows how to materialise into a ChaosEngine.
const (
	ExperimentPodDelete         = "pod-delete"
	ExperimentPodNetworkLatency = "pod-network-latency"
)

var experimentTypes = []ExperimentType{
	{
		Name:        ExperimentPodDelete,
		Description: "Randomly deletes target pods to verify the workload recovers.",
		Params: map[string]string{
			"TOTAL_CHAOS_DURATION": "30",
			"CHAOS_INTERVAL":       "10",
			"PODS_AFFECTED_PERC":   "50",
		},
	},
	{
		Name:        ExperimentPodNetworkLatency,
		Description: "Injects network latency into target pods.",
		Params: map[string]string{
			"TOTAL_CHAOS_DURATION": "60",
			"NETWORK_LATENCY":      "2000",
		},
	},
}

const maxBodyBytes = 64 * 1024

// Experiment is a single chaos run targeting a workload in a customer cluster.
// The control plane never touches the cluster — it enqueues a command for the
// outpost and records the verdict the outpost reports back.
type Experiment struct {
	ExperimentID   string            `json:"experiment_id"    gorm:"column:experiment_id;primaryKey"`
	UserID         string            `json:"user_id"          gorm:"column:user_id"`
	OrgID          string            `json:"org_id"           gorm:"column:org_id;default:''"`
	OutpostID      string            `json:"outpost_id"       gorm:"column:outpost_id"`
	ExperimentType string            `json:"experiment_type"  gorm:"column:experiment_type"`
	TargetAppNS    string            `json:"target_app_ns"    gorm:"column:target_app_ns"`
	TargetAppLabel string            `json:"target_app_label" gorm:"column:target_app_label"`
	TargetAppKind  string            `json:"target_app_kind"  gorm:"column:target_app_kind;default:'deployment'"`
	EngineName     string            `json:"engine_name"      gorm:"column:engine_name"`
	Params         map[string]string `json:"params"           gorm:"column:params;serializer:json"`
	Status         string            `json:"status"           gorm:"column:status;default:'pending'"`
	Verdict        string            `json:"verdict"          gorm:"column:verdict;default:''"`
	FailStep       string            `json:"fail_step"        gorm:"column:fail_step;default:''"`
	ProbeSuccess   string            `json:"probe_success"    gorm:"column:probe_success;default:''"`
	Active         bool              `json:"-"                gorm:"column:active;default:true"`
	CreatedAt      time.Time         `json:"created_at"       gorm:"column:created_at"`
	StartedAt      *time.Time        `json:"started_at,omitempty" gorm:"column:started_at"`
	EndedAt        *time.Time        `json:"ended_at,omitempty"   gorm:"column:ended_at"`
}

func (Experiment) TableName() string { return "experiments" }

// ExperimentType is a catalog entry returned by GET /experiment-types.
type ExperimentType struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Params      map[string]string `json:"params"`
}

type createExperimentRequest struct {
	OutpostID      string            `json:"outpost_id"`
	ExperimentType string            `json:"experiment_type"`
	TargetAppNS    string            `json:"target_app_ns"`
	TargetAppLabel string            `json:"target_app_label"`
	TargetAppKind  string            `json:"target_app_kind"`
	Params         map[string]string `json:"params"`
}
