package provider

import (
	"context"
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
	_ resource.Resource                = (*stepResource)(nil)
	_ resource.ResourceWithConfigure   = (*stepResource)(nil)
	_ resource.ResourceWithImportState = (*stepResource)(nil)
)

const stepsPath = "/workflows/steps"

type stepResource struct{ client *Client }

func newStepResource() resource.Resource { return &stepResource{} }

type stepModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Action      types.String `tfsdk:"action"`
	Description types.String `tfsdk:"description"`
	With        types.Map    `tfsdk:"with"`
	Timeout     types.Int64  `tfsdk:"timeout"`
	CreatedBy   types.String `tfsdk:"created_by"`
	OrgID       types.String `tfsdk:"org_id"`
	Active      types.Bool   `tfsdk:"active"`
	CreatedAt   types.String `tfsdk:"created_at"`
	UpdatedAt   types.String `tfsdk:"updated_at"`
}

type stepRequest struct {
	Name        string            `json:"name"`
	Action      string            `json:"action"`
	Description string            `json:"description,omitempty"`
	With        map[string]string `json:"with,omitempty"`
	Timeout     int64             `json:"timeout,omitempty"`
}

type stepResponse struct {
	StepID      string `json:"step_id"`
	Name        string `json:"name"`
	Action      string `json:"action"`
	Description string `json:"description"`
	Timeout     int64  `json:"timeout"`
	CreatedBy   string `json:"created_by"`
	OrgID       string `json:"org_id"`
	Active      bool   `json:"active"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func (r *stepResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_step"
}

func (r *stepResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A reusable workflows step definition: a named action plus its `with` config, referenceable from pipelines.",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "Step ID (UUID).", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"name":        schema.StringAttribute{Required: true, MarkdownDescription: "Step name (unique per user/org)."},
			"action":      schema.StringAttribute{Required: true, MarkdownDescription: "The action this step runs, e.g. `forge/run`, `http`."},
			"description": schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString(""), MarkdownDescription: "Human-readable description."},
			"with":        schema.MapAttribute{ElementType: types.StringType, Optional: true, MarkdownDescription: "The step's action inputs."},
			"timeout":     schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: "Per-step timeout in seconds (0/unset → the service default of 30)."},
			"created_by":  schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"org_id":      schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"active":      schema.BoolAttribute{Computed: true},
			"created_at":  schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"updated_at":  schema.StringAttribute{Computed: true},
		},
	}
}

func (r *stepResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	client, diags := clientFromProviderData(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	r.client = client
}

func (m stepModel) toRequest(ctx context.Context) (stepRequest, diag.Diagnostics) {
	var diags diag.Diagnostics
	with := map[string]string{}
	if !m.With.IsNull() && !m.With.IsUnknown() {
		diags.Append(m.With.ElementsAs(ctx, &with, false)...)
	}
	return stepRequest{
		Name:        m.Name.ValueString(),
		Action:      m.Action.ValueString(),
		Description: m.Description.ValueString(),
		With:        with,
		Timeout:     m.Timeout.ValueInt64(),
	}, diags
}

// fromResponse refreshes computed/server fields; `with` is kept from state to avoid
// diff churn from server-side normalization of the action inputs.
func (m *stepModel) fromResponse(out stepResponse) {
	m.ID = types.StringValue(out.StepID)
	m.Name = types.StringValue(out.Name)
	m.Action = types.StringValue(out.Action)
	m.Description = types.StringValue(out.Description)
	m.Timeout = types.Int64Value(out.Timeout)
	m.CreatedBy = types.StringValue(out.CreatedBy)
	m.OrgID = types.StringValue(out.OrgID)
	m.Active = types.BoolValue(out.Active)
	m.CreatedAt = types.StringValue(out.CreatedAt)
	m.UpdatedAt = types.StringValue(out.UpdatedAt)
}

func (r *stepResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan stepModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body, d := plan.toRequest(ctx)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out stepResponse
	_, d = r.client.do2xx(ctx, "Create step failed", "create", http.MethodPost, stepsPath, body, &out, http.StatusCreated)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.fromResponse(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *stepResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state stepModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out stepResponse
	notFound, d := r.client.do2xx(ctx, "Read step failed", "read", http.MethodGet, stepsPath+"/"+state.ID.ValueString(), nil, &out, http.StatusOK)
	if notFound {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	state.fromResponse(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *stepResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan stepModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body, d := plan.toRequest(ctx)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out stepResponse
	_, d = r.client.do2xx(ctx, "Update step failed", "update", http.MethodPut, stepsPath+"/"+plan.ID.ValueString(), body, &out, http.StatusOK)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.fromResponse(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *stepResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state stepModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	notFound, d := r.client.do2xx(ctx, "Delete step failed", "delete", http.MethodDelete, stepsPath+"/"+state.ID.ValueString(), nil, nil, http.StatusNoContent)
	if notFound {
		return
	}
	resp.Diagnostics.Append(d...)
}

func (r *stepResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
