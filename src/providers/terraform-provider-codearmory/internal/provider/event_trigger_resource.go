package provider

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*eventTriggerResource)(nil)
	_ resource.ResourceWithConfigure   = (*eventTriggerResource)(nil)
	_ resource.ResourceWithImportState = (*eventTriggerResource)(nil)
)

type eventTriggerResource struct {
	client *Client
}

func newEventTriggerResource() resource.Resource { return &eventTriggerResource{} }

// eventTriggerModel carries `match` and `actions` as JSON strings rather than nested
// Terraform objects.
//
// Both are recursive/heterogeneous by design — a filter node contains filter nodes, and each
// action's `config` is kind-specific — and the plugin framework has no way to express either
// as a typed schema without flattening the grammar down to whatever subset was modelled. A
// JSON string keeps every filter and action expressible on day one, and Terraform's own
// jsonencode() makes it readable in HCL.
type eventTriggerModel struct {
	ID        types.String `tfsdk:"id"`
	Name      types.String `tfsdk:"name"`
	Match     types.String `tfsdk:"match"`
	Actions   types.String `tfsdk:"actions"`
	Enabled   types.Bool   `tfsdk:"enabled"`
	CreatedBy types.String `tfsdk:"created_by"`
	OrgID     types.String `tfsdk:"org_id"`
	CreatedAt types.String `tfsdk:"created_at"`
	UpdatedAt types.String `tfsdk:"updated_at"`
}

type eventTriggerRequest struct {
	Name    string          `json:"name"`
	Match   json.RawMessage `json:"match"`
	Actions json.RawMessage `json:"actions"`
	Enabled bool            `json:"enabled"`
}

type eventTriggerResponse struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Match     json.RawMessage `json:"match"`
	Actions   json.RawMessage `json:"actions"`
	Enabled   bool            `json:"enabled"`
	CreatedBy string          `json:"created_by"`
	OrgID     string          `json:"org_id"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
}

func (r *eventTriggerResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_event_trigger"
}

func (r *eventTriggerResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An events trigger: when an event matches the filter, its actions dispatch " +
			"(run a pipeline, open a ticket, notify, call a webhook, enqueue an outpost command).\n\n" +
			"Replaces the removed `codearmory_hook_rule`. A hook rule could only match a git-shaped " +
			"tuple and only trigger a workflow; a trigger filters any field of any event and dispatches " +
			"any action.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Trigger ID (UUID).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Trigger name.",
			},
			"match": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Filter document, JSON-encoded. Either a group (`all`/`any`/`not`) or a " +
					"leaf (`field`/`op`/`value`); groups nest. Fields are dotted paths into the event " +
					"(`type`, `subject`, `actor.org_id`, `data.ref`).\n\n" +
					"```hcl\n" +
					"match = jsonencode({\n" +
					"  all = [\n" +
					"    { field = \"type\",     op = \"eq\", value = \"repo.push\" },\n" +
					"    { field = \"subject\",  op = \"eq\", value = \"myorg/myrepo\" },\n" +
					"    { field = \"data.ref\", op = \"eq\", value = \"main\" },\n" +
					"  ]\n" +
					"})\n" +
					"```",
			},
			"actions": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Actions to dispatch, JSON-encoded as a list of `{kind, config}`. " +
					"String values in `config` are templated against the event: `{{ data.commit }}`.\n\n" +
					"```hcl\n" +
					"actions = jsonencode([{\n" +
					"  kind   = \"run_pipeline\"\n" +
					"  config = {\n" +
					"    pipeline_id = codearmory_pipeline.deploy.id\n" +
					"    inputs      = { IMAGE_TAG = \"{{ data.commit }}\" }\n" +
					"  }\n" +
					"}])\n" +
					"```",
			},
			"enabled": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
				MarkdownDescription: "Whether the trigger dispatches. Disabled triggers are never evaluated.",
			},
			"created_by": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "User ID that created the trigger.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"org_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Org the trigger belongs to.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Creation timestamp (RFC3339).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"updated_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Last-update timestamp (RFC3339).",
			},
		},
	}
}

func (r *eventTriggerResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	client, diags := clientFromProviderData(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	r.client = client
}

func (m eventTriggerModel) toRequest() (eventTriggerRequest, diag.Diagnostics) {
	var diags diag.Diagnostics
	// Validate here rather than letting the service reject it: a JSON error caught at plan
	// time names the attribute, where a 400 from the API would not.
	match := json.RawMessage(m.Match.ValueString())
	if !json.Valid(match) {
		diags.AddAttributeError(path.Root("match"), "Invalid match JSON",
			"match must be a JSON-encoded filter document; use jsonencode({...}).")
	}
	actions := json.RawMessage(m.Actions.ValueString())
	if !json.Valid(actions) {
		diags.AddAttributeError(path.Root("actions"), "Invalid actions JSON",
			"actions must be a JSON-encoded list of {kind, config}; use jsonencode([...]).")
	}
	return eventTriggerRequest{
		Name:    m.Name.ValueString(),
		Match:   match,
		Actions: actions,
		Enabled: m.Enabled.ValueBool(),
	}, diags
}

// fromResponse populates server-derived fields.
//
// match and actions are deliberately NOT overwritten from the response: the service round-trips
// them through a JSON column, so the bytes it returns are semantically equal but rarely
// byte-identical to the configured string (key order, whitespace). Writing them back would
// produce a permanent diff on every plan.
func (m *eventTriggerModel) fromResponse(out eventTriggerResponse) {
	m.ID = types.StringValue(out.ID)
	m.Name = types.StringValue(out.Name)
	m.Enabled = types.BoolValue(out.Enabled)
	m.CreatedBy = types.StringValue(out.CreatedBy)
	m.OrgID = types.StringValue(out.OrgID)
	m.CreatedAt = types.StringValue(out.CreatedAt)
	m.UpdatedAt = types.StringValue(out.UpdatedAt)
}

func (r *eventTriggerResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan eventTriggerModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	apiReq, diags := plan.toRequest()
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out eventTriggerResponse
	_, d := r.client.do2xx(ctx, "Create event trigger failed", "create", http.MethodPost, "/events/triggers", apiReq, &out, http.StatusCreated)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.fromResponse(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *eventTriggerResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state eventTriggerModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out eventTriggerResponse
	notFound, d := r.client.do2xx(ctx, "Read event trigger failed", "read", http.MethodGet, "/events/triggers/"+state.ID.ValueString(), nil, &out, http.StatusOK)
	if notFound {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	state.fromResponse(out)
	// On import there is no prior config, so the JSON attributes would otherwise stay null and
	// fail the "required attribute" check. Seed them from the server exactly once.
	if state.Match.IsNull() {
		state.Match = types.StringValue(string(out.Match))
	}
	if state.Actions.IsNull() {
		state.Actions = types.StringValue(string(out.Actions))
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *eventTriggerResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan eventTriggerModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	apiReq, diags := plan.toRequest()
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out eventTriggerResponse
	_, d := r.client.do2xx(ctx, "Update event trigger failed", "update", http.MethodPut, "/events/triggers/"+plan.ID.ValueString(), apiReq, &out, http.StatusOK)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.fromResponse(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *eventTriggerResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state eventTriggerModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// 204 and 404 both mean "gone"; do2xx maps 404 to notFound.
	notFound, d := r.client.do2xx(ctx, "Delete event trigger failed", "delete", http.MethodDelete, "/events/triggers/"+state.ID.ValueString(), nil, nil, http.StatusNoContent)
	if notFound {
		return
	}
	resp.Diagnostics.Append(d...)
}

func (r *eventTriggerResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
