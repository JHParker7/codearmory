package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*hookRuleResource)(nil)
	_ resource.ResourceWithConfigure   = (*hookRuleResource)(nil)
	_ resource.ResourceWithImportState = (*hookRuleResource)(nil)
)

type hookRuleResource struct {
	client *Client
}

func newHookRuleResource() resource.Resource { return &hookRuleResource{} }

type hookRuleModel struct {
	ID           types.String `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	Source       types.String `tfsdk:"source"`
	Events       types.List   `tfsdk:"events"`
	RefFilter    types.String `tfsdk:"ref_filter"`
	RepoFilter   types.String `tfsdk:"repo_filter"`
	WorkflowID   types.String `tfsdk:"workflow_id"`
	Secret       types.String `tfsdk:"secret"`
	InputMapping types.Map    `tfsdk:"input_mapping"`
	CreatedBy    types.String `tfsdk:"created_by"`
	OrgID        types.String `tfsdk:"org_id"`
	Active       types.Bool   `tfsdk:"active"`
	CreatedAt    types.String `tfsdk:"created_at"`
}

// hookRuleRequest is the create/update body. Secret is a pointer so it can be
// omitted on update (nil = leave unchanged) per the hooks API contract.
type hookRuleRequest struct {
	Name         string            `json:"name"`
	Source       string            `json:"source"`
	Events       []string          `json:"events"`
	RefFilter    string            `json:"ref_filter"`
	RepoFilter   string            `json:"repo_filter"`
	WorkflowID   string            `json:"workflow_id"`
	Secret       *string           `json:"secret,omitempty"`
	InputMapping map[string]string `json:"input_mapping"`
}

// hookRuleResponse mirrors the PipelineRule JSON. Secret is never returned.
type hookRuleResponse struct {
	RuleID       string            `json:"rule_id"`
	Name         string            `json:"name"`
	Source       string            `json:"source"`
	Events       []string          `json:"events"`
	RefFilter    string            `json:"ref_filter"`
	RepoFilter   string            `json:"repo_filter"`
	WorkflowID   string            `json:"workflow_id"`
	InputMapping map[string]string `json:"input_mapping"`
	CreatedBy    string            `json:"created_by"`
	OrgID        string            `json:"org_id"`
	Active       bool              `json:"active"`
	CreatedAt    string            `json:"created_at"`
}

func (r *hookRuleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_hook_rule"
}

func (r *hookRuleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A hooks pipeline rule: triggers a pipeline run when a matching webhook event arrives.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Rule ID (UUID).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Rule name.",
			},
			"source": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Event source identifier (e.g. `myorg/myrepo`).",
			},
			"events": schema.ListAttribute{
				Required:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Event types to match, e.g. `[\"push\", \"pull_request\"]`.",
			},
			"ref_filter": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(""),
				MarkdownDescription: "Optional git ref filter (e.g. `refs/heads/main` or `refs/heads/*`). Empty matches all.",
			},
			"repo_filter": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(""),
				MarkdownDescription: "Optional repository filter, matched against the payload's `repo` path with the same glob semantics as `ref_filter` (e.g. `admin/codearmory`). Empty matches EVERY repo, which is rarely what you want for a source that hosts many: a git backend emits every repo's push under one shared source, so without this a push to one repo fires every pipeline subscribed to that backend.",
			},
			"workflow_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Pipeline (workflow) ID to trigger. Must belong to the same org as the caller.",
			},
			"secret": schema.StringAttribute{
				Required:  true,
				Sensitive: true,
				MarkdownDescription: "HMAC secret used to verify inbound webhook signatures. Write-only: the API never returns it, " +
					"so out-of-band changes are not detected and `import` cannot recover it.",
			},
			"input_mapping": schema.MapAttribute{
				Optional:            true,
				Computed:            true,
				ElementType:         types.StringType,
				Default:             mapdefault.StaticValue(types.MapValueMust(types.StringType, map[string]attr.Value{})),
				MarkdownDescription: "Maps pipeline input keys to webhook payload fields.",
			},
			"created_by": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "User ID that created the rule.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"org_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Org the rule belongs to.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"active": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether the rule is active.",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Creation timestamp (RFC3339).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *hookRuleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	client, diags := clientFromProviderData(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	r.client = client
}

func (m hookRuleModel) toRequest(ctx context.Context) (hookRuleRequest, diag.Diagnostics) {
	var diags diag.Diagnostics
	var events []string
	diags.Append(m.Events.ElementsAs(ctx, &events, false)...)

	mapping := map[string]string{}
	if !m.InputMapping.IsNull() && !m.InputMapping.IsUnknown() {
		diags.Append(m.InputMapping.ElementsAs(ctx, &mapping, false)...)
	}

	secret := m.Secret.ValueString()
	return hookRuleRequest{
		Name:         m.Name.ValueString(),
		Source:       m.Source.ValueString(),
		Events:       events,
		RefFilter:    m.RefFilter.ValueString(),
		RepoFilter:   m.RepoFilter.ValueString(),
		WorkflowID:   m.WorkflowID.ValueString(),
		Secret:       &secret,
		InputMapping: mapping,
	}, diags
}

// fromResponse populates server-derived fields. It deliberately does NOT touch
// Secret, which the API never returns — the configured value stays in state.
func (m *hookRuleModel) fromResponse(ctx context.Context, out hookRuleResponse) diag.Diagnostics {
	var diags diag.Diagnostics
	m.ID = types.StringValue(out.RuleID)
	m.Name = types.StringValue(out.Name)
	m.Source = types.StringValue(out.Source)

	events, d := types.ListValueFrom(ctx, types.StringType, out.Events)
	diags.Append(d...)
	m.Events = events

	m.RefFilter = types.StringValue(out.RefFilter)
	m.RepoFilter = types.StringValue(out.RepoFilter)
	m.WorkflowID = types.StringValue(out.WorkflowID)

	mapping := out.InputMapping
	if mapping == nil {
		mapping = map[string]string{}
	}
	mp, d := types.MapValueFrom(ctx, types.StringType, mapping)
	diags.Append(d...)
	m.InputMapping = mp

	m.CreatedBy = types.StringValue(out.CreatedBy)
	m.OrgID = types.StringValue(out.OrgID)
	m.Active = types.BoolValue(out.Active)
	m.CreatedAt = types.StringValue(out.CreatedAt)
	return diags
}

func (r *hookRuleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan hookRuleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	apiReq, diags := plan.toRequest(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out hookRuleResponse
	_, d := r.client.do2xx(ctx, "Create hook rule failed", "create", http.MethodPost, "/hooks/rules", apiReq, &out, http.StatusCreated)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(plan.fromResponse(ctx, out)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *hookRuleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state hookRuleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out hookRuleResponse
	notFound, d := r.client.do2xx(ctx, "Read hook rule failed", "read", http.MethodGet, "/hooks/rules/"+state.ID.ValueString(), nil, &out, http.StatusOK)
	if notFound {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Secret is preserved from prior state (the API never returns it).
	resp.Diagnostics.Append(state.fromResponse(ctx, out)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *hookRuleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan hookRuleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	apiReq, diags := plan.toRequest(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out hookRuleResponse
	_, d := r.client.do2xx(ctx, "Update hook rule failed", "update", http.MethodPut, "/hooks/rules/"+plan.ID.ValueString(), apiReq, &out, http.StatusOK)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(plan.fromResponse(ctx, out)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *hookRuleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state hookRuleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// 204 and 404 both mean "gone"; do2xx maps 404 to notFound.
	notFound, d := r.client.do2xx(ctx, "Delete hook rule failed", "delete", http.MethodDelete, "/hooks/rules/"+state.ID.ValueString(), nil, nil, http.StatusNoContent)
	if notFound {
		return
	}
	resp.Diagnostics.Append(d...)
}

func (r *hookRuleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Import by rule_id. `secret` is write-only and cannot be recovered — set it in
	// config; the next apply will re-send it.
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
