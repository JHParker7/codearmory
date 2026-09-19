package provider

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*pipelineResource)(nil)
	_ resource.ResourceWithConfigure   = (*pipelineResource)(nil)
	_ resource.ResourceWithImportState = (*pipelineResource)(nil)
)

// Pipelines are the workflows service's create/read/update/delete surface, addressed
// through the gateway as /workflows/pipelines[/{id}].
const pipelinesPath = "/workflows/pipelines"

type pipelineResource struct {
	client *Client
}

func newPipelineResource() resource.Resource { return &pipelineResource{} }

type pipelineModel struct {
	ID             types.String `tfsdk:"id"`
	Name           types.String `tfsdk:"name"`
	Description    types.String `tfsdk:"description"`
	Project        types.String `tfsdk:"project"`
	Steps          types.List   `tfsdk:"step"`
	DefinitionJSON types.String `tfsdk:"definition_json"`
	CreatedBy      types.String `tfsdk:"created_by"`
	OrgID          types.String `tfsdk:"org_id"`
	Active         types.Bool   `tfsdk:"active"`
	CreatedAt      types.String `tfsdk:"created_at"`
}

type pipelineStepModel struct {
	Name     types.String `tfsdk:"name"`
	Action   types.String `tfsdk:"action"`
	Timeout  types.Int64  `tfsdk:"timeout"`
	With     types.Map    `tfsdk:"with"`
	WithJSON types.String `tfsdk:"with_json"`
}

// pipelineStepRequest is one inline step in the create/update body: an action plus its
// full `with` config (mirrors workflows' WorkflowStepRef for an inline step). With is
// map[string]any so nested forge config (checkout, build, volumes, secret_refs) — set
// via with_json — round-trips as real objects, not stringified.
type pipelineStepRequest struct {
	Name    string         `json:"name,omitempty"`
	Action  string         `json:"action,omitempty"`
	Timeout int64          `json:"timeout,omitempty"`
	With    map[string]any `json:"with,omitempty"`
}

type createWorkflowRequest struct {
	Name        string                `json:"name"`
	Description string                `json:"description,omitempty"`
	Project     string                `json:"project,omitempty"`
	Steps       []pipelineStepRequest `json:"steps,omitempty"`
}

type workflowResponse struct {
	WorkflowID  string `json:"workflow_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Project     string `json:"project"`
	CreatedBy   string `json:"created_by"`
	OrgID       string `json:"org_id"`
	Active      bool   `json:"active"`
	CreatedAt   string `json:"created_at"`
}

func (r *pipelineResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_pipeline"
}

func (r *pipelineResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A workflows pipeline: an ordered set of inline steps that run as authenticated calls to platform services (e.g. `forge/run`). Steps run in declaration order.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Pipeline (workflow) ID (UUID).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Pipeline name. Required with `step`; taken from `definition_json` when that is used.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"description": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(""),
				MarkdownDescription: "Human-readable description.",
			},
			"project": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(""),
				MarkdownDescription: "Project label this pipeline belongs to (a view filter).",
			},
			"created_by": schema.StringAttribute{Computed: true, MarkdownDescription: "User ID that created the pipeline.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"org_id":     schema.StringAttribute{Computed: true, MarkdownDescription: "Owning org ID, if any.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"active":     schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the pipeline is active."},
			"created_at": schema.StringAttribute{Computed: true, MarkdownDescription: "Creation timestamp (RFC3339).", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"definition_json": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "A complete pipeline definition as a JSON object string (name/description/inputs/steps/maps/routes/state_machine), sent verbatim. Use for pipeline-as-code from a repo YAML: `jsonencode(yamldecode(file(\"pipeline.yaml\")))`. Mutually exclusive with `step`.",
			},
			"step": schema.ListNestedAttribute{
				Optional:            true,
				MarkdownDescription: "The pipeline's steps, in run order. Mutually exclusive with `definition_json`.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name":      schema.StringAttribute{Required: true, MarkdownDescription: "Step name (referenced as ${steps.<name>.output})."},
						"action":    schema.StringAttribute{Required: true, MarkdownDescription: "The action the step runs, e.g. `forge/run`, `forge/build-image`."},
						"timeout":   schema.Int64Attribute{Optional: true, MarkdownDescription: "Per-step timeout in seconds (0 / unset = service default)."},
						"with":      schema.MapAttribute{ElementType: types.StringType, Optional: true, MarkdownDescription: "Simple string config for the action (e.g. `image`, `run` for `forge/run`)."},
						"with_json": schema.StringAttribute{Optional: true, MarkdownDescription: "Advanced config as a JSON object string, merged over `with`. Use for nested forge config — `checkout`, `build`, `volumes`, `secret_refs` — that can't be expressed as flat strings."},
					},
				},
			},
		},
	}
}

func (r *pipelineResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	client, diags := clientFromProviderData(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	r.client = client
}

// toRequest builds the create/update body from the plan. When definition_json is set
// it is sent verbatim (the whole pipeline document); otherwise the body is assembled
// from name + the inline steps.
func (m pipelineModel) toRequest(ctx context.Context) (any, diag.Diagnostics) {
	var diags diag.Diagnostics
	if !m.DefinitionJSON.IsNull() && !m.DefinitionJSON.IsUnknown() && m.DefinitionJSON.ValueString() != "" {
		var body map[string]any
		if err := json.Unmarshal([]byte(m.DefinitionJSON.ValueString()), &body); err != nil {
			diags.AddError("Invalid definition_json", err.Error())
		}
		return body, diags
	}
	var steps []pipelineStepModel
	diags.Append(m.Steps.ElementsAs(ctx, &steps, false)...)
	if diags.HasError() {
		return createWorkflowRequest{}, diags
	}
	reqSteps := make([]pipelineStepRequest, 0, len(steps))
	for _, s := range steps {
		with := map[string]any{}
		if !s.With.IsNull() && !s.With.IsUnknown() {
			strs := map[string]string{}
			diags.Append(s.With.ElementsAs(ctx, &strs, false)...)
			for k, v := range strs {
				with[k] = v
			}
		}
		// with_json carries nested config (checkout/build/volumes/secret_refs); its keys
		// override the flat `with` map.
		if !s.WithJSON.IsNull() && !s.WithJSON.IsUnknown() && s.WithJSON.ValueString() != "" {
			var nested map[string]any
			if err := json.Unmarshal([]byte(s.WithJSON.ValueString()), &nested); err != nil {
				diags.AddError("Invalid with_json", err.Error())
				return createWorkflowRequest{}, diags
			}
			for k, v := range nested {
				with[k] = v
			}
		}
		reqSteps = append(reqSteps, pipelineStepRequest{
			Name:    s.Name.ValueString(),
			Action:  s.Action.ValueString(),
			Timeout: s.Timeout.ValueInt64(),
			With:    with,
		})
	}
	return createWorkflowRequest{
		Name:        m.Name.ValueString(),
		Description: m.Description.ValueString(),
		Project:     m.Project.ValueString(),
		Steps:       reqSteps,
	}, diags
}

// fromResponse refreshes only the server-owned computed fields; the step list is kept
// from configuration/state to avoid perpetual diffs from server-side normalization.
func (m *pipelineModel) fromResponse(out workflowResponse) {
	m.ID = types.StringValue(out.WorkflowID)
	m.CreatedBy = types.StringValue(out.CreatedBy)
	m.OrgID = types.StringValue(out.OrgID)
	m.Active = types.BoolValue(out.Active)
	m.CreatedAt = types.StringValue(out.CreatedAt)
	// In definition_json mode the document owns name/description/project; Terraform
	// planned them from prior state, so overwriting from the response would be an
	// "inconsistent result". Fill `name` only when it's still unknown (create), and
	// leave description/project on their planned (defaulted/prior) values.
	if !m.DefinitionJSON.IsNull() && !m.DefinitionJSON.IsUnknown() && m.DefinitionJSON.ValueString() != "" {
		if m.Name.IsUnknown() {
			m.Name = types.StringValue(out.Name)
		}
		return
	}
	m.Name = types.StringValue(out.Name)
	m.Description = types.StringValue(out.Description)
	m.Project = types.StringValue(out.Project)
}

func (r *pipelineResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan pipelineModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body, diags := plan.toRequest(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out workflowResponse
	_, d := r.client.do2xx(ctx, "Create pipeline failed", "create", http.MethodPost, pipelinesPath, body, &out, http.StatusCreated)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.fromResponse(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *pipelineResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state pipelineModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out workflowResponse
	notFound, d := r.client.do2xx(ctx, "Read pipeline failed", "read", http.MethodGet, pipelinesPath+"/"+state.ID.ValueString(), nil, &out, http.StatusOK)
	if notFound {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Steps are kept from state (not refreshed) to avoid diff churn.
	state.fromResponse(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *pipelineResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan pipelineModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body, diags := plan.toRequest(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out workflowResponse
	_, d := r.client.do2xx(ctx, "Update pipeline failed", "update", http.MethodPut, pipelinesPath+"/"+plan.ID.ValueString(), body, &out, http.StatusOK)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.fromResponse(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *pipelineResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state pipelineModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	notFound, d := r.client.do2xx(ctx, "Delete pipeline failed", "delete", http.MethodDelete, pipelinesPath+"/"+state.ID.ValueString(), nil, nil, http.StatusNoContent)
	if notFound {
		return
	}
	resp.Diagnostics.Append(d...)
}

func (r *pipelineResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
