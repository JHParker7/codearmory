package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*outpostResource)(nil)
	_ resource.ResourceWithConfigure   = (*outpostResource)(nil)
	_ resource.ResourceWithImportState = (*outpostResource)(nil)
)

const outpostsPath = "/outpost-gateway/outposts"

type outpostResource struct{ client *Client }

func newOutpostResource() resource.Resource { return &outpostResource{} }

type outpostModel struct {
	ID              types.String `tfsdk:"id"`
	Name            types.String `tfsdk:"name"`
	Modules         types.List   `tfsdk:"modules"`
	OrgID           types.String `tfsdk:"org_id"`
	UserID          types.String `tfsdk:"user_id"`
	Status          types.String `tfsdk:"status"`
	LastSeenAt      types.String `tfsdk:"last_seen_at"`
	CreatedAt       types.String `tfsdk:"created_at"`
	UpdatedAt       types.String `tfsdk:"updated_at"`
	EnrollmentToken types.String `tfsdk:"enrollment_token"`
}

type createOutpostRequest struct {
	Name    string   `json:"name"`
	Modules []string `json:"modules,omitempty"`
}

type outpostResponse struct {
	OutpostID       string  `json:"outpost_id"`
	OrgID           string  `json:"org_id"`
	UserID          string  `json:"user_id"`
	Name            string  `json:"name"`
	Modules         string  `json:"modules"`
	Status          string  `json:"status"`
	LastSeenAt      *string `json:"last_seen_at"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
	EnrollmentToken string  `json:"enrollment_token"`
}

func (r *outpostResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_outpost"
}

func (r *outpostResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An outpost registration on the outpost-gateway: a user-deployed in-cluster agent that runs the named integration modules. Immutable — changing name or modules recreates it. The `enrollment_token` is returned once at creation.",
		Attributes: map[string]schema.Attribute{
			"id":   schema.StringAttribute{Computed: true, MarkdownDescription: "Outpost ID (UUID).", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"name": schema.StringAttribute{Required: true, MarkdownDescription: "Outpost name (unique per scope; immutable).", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"modules": schema.ListAttribute{
				ElementType:         types.StringType,
				Optional:            true,
				MarkdownDescription: "Integration modules the outpost runs, e.g. `chaos`, `argo` (immutable).",
				PlanModifiers:       []planmodifier.List{listplanmodifier.RequiresReplace()},
			},
			"org_id":           schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"user_id":          schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"status":           schema.StringAttribute{Computed: true, MarkdownDescription: "Connection status (`pending` → `connected`)."},
			"last_seen_at":     schema.StringAttribute{Computed: true, MarkdownDescription: "Last time the outpost checked in."},
			"created_at":       schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"updated_at":       schema.StringAttribute{Computed: true},
			"enrollment_token": schema.StringAttribute{Computed: true, Sensitive: true, MarkdownDescription: "Single-use token the outpost self-registers with. Returned ONLY at creation and never again.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
		},
	}
}

func (r *outpostResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	client, diags := clientFromProviderData(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	r.client = client
}

// fromResponse refreshes server fields. modules is kept from config (the response
// returns it CSV-encoded, and it is immutable anyway); enrollment_token is kept from
// state on read (never returned after creation).
func (m *outpostModel) fromResponse(out outpostResponse) {
	m.ID = types.StringValue(out.OutpostID)
	m.Name = types.StringValue(out.Name)
	m.OrgID = types.StringValue(out.OrgID)
	m.UserID = types.StringValue(out.UserID)
	m.Status = types.StringValue(out.Status)
	m.LastSeenAt = nullableString(out.LastSeenAt)
	m.CreatedAt = types.StringValue(out.CreatedAt)
	m.UpdatedAt = types.StringValue(out.UpdatedAt)
}

func (r *outpostResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan outpostModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var modules []string
	if !plan.Modules.IsNull() && !plan.Modules.IsUnknown() {
		resp.Diagnostics.Append(plan.Modules.ElementsAs(ctx, &modules, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}
	var out outpostResponse
	_, d := r.client.do2xx(ctx, "Create outpost failed", "create", http.MethodPost, outpostsPath, createOutpostRequest{Name: plan.Name.ValueString(), Modules: modules}, &out, http.StatusCreated)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.fromResponse(out)
	plan.EnrollmentToken = types.StringValue(out.EnrollmentToken) // captured once, here only
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *outpostResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state outpostModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out outpostResponse
	notFound, d := r.client.do2xx(ctx, "Read outpost failed", "read", http.MethodGet, outpostsPath+"/"+state.ID.ValueString(), nil, &out, http.StatusOK)
	if notFound {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	state.fromResponse(out) // modules + enrollment_token kept from state
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is unreachable — every mutable attribute forces replacement — but the
// interface requires it.
func (r *outpostResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan outpostModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *outpostResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state outpostModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	notFound, d := r.client.do2xx(ctx, "Delete outpost failed", "delete", http.MethodDelete, outpostsPath+"/"+state.ID.ValueString(), nil, nil, http.StatusNoContent)
	if notFound {
		return
	}
	resp.Diagnostics.Append(d...)
}

func (r *outpostResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
