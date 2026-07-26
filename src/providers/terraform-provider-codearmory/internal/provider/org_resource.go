package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*orgResource)(nil)
	_ resource.ResourceWithConfigure   = (*orgResource)(nil)
	_ resource.ResourceWithImportState = (*orgResource)(nil)
)

const orgsPath = "/gatekeeper/orgs"

type orgResource struct{ client *Client }

func newOrgResource() resource.Resource { return &orgResource{} }

type orgModel struct {
	ID        types.String `tfsdk:"id"`
	OrgName   types.String `tfsdk:"org_name"`
	OwnerID   types.String `tfsdk:"owner_id"`
	Active    types.Bool   `tfsdk:"active"`
	CreatedAt types.String `tfsdk:"created_at"`
	UpdatedAt types.String `tfsdk:"updated_at"`
}

type orgRequest struct {
	OrgName string `json:"org_name"`
}

type orgResponse struct {
	OrgID     string `json:"org_id"`
	OrgName   string `json:"org_name"`
	OwnerID   string `json:"owner_id"`
	Active    bool   `json:"active"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func (r *orgResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org"
}

func (r *orgResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A gatekeeper organization: the ownership/permission boundary that teams, roles and org-scoped resources belong to.",
		Attributes: map[string]schema.Attribute{
			"id":         schema.StringAttribute{Computed: true, MarkdownDescription: "Org ID (UUID).", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"org_name":   schema.StringAttribute{Required: true, MarkdownDescription: "Organization name."},
			"owner_id":   schema.StringAttribute{Computed: true, MarkdownDescription: "User ID of the org owner.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"active":     schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the org is active."},
			"created_at": schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"updated_at": schema.StringAttribute{Computed: true},
		},
	}
}

func (r *orgResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	client, diags := clientFromProviderData(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	r.client = client
}

func (m *orgModel) fromResponse(out orgResponse) {
	m.ID = types.StringValue(out.OrgID)
	m.OrgName = types.StringValue(out.OrgName)
	m.OwnerID = types.StringValue(out.OwnerID)
	m.Active = types.BoolValue(out.Active)
	m.CreatedAt = types.StringValue(out.CreatedAt)
	m.UpdatedAt = types.StringValue(out.UpdatedAt)
}

func (r *orgResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan orgModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out orgResponse
	_, d := r.client.do2xx(ctx, "Create org failed", "create", http.MethodPost, orgsPath, orgRequest{OrgName: plan.OrgName.ValueString()}, &out, http.StatusCreated)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.fromResponse(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *orgResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state orgModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out orgResponse
	notFound, d := r.client.do2xx(ctx, "Read org failed", "read", http.MethodGet, orgsPath+"/"+state.ID.ValueString(), nil, &out, http.StatusOK)
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

func (r *orgResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan orgModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out orgResponse
	_, d := r.client.do2xx(ctx, "Update org failed", "update", http.MethodPut, orgsPath+"/"+plan.ID.ValueString(), orgRequest{OrgName: plan.OrgName.ValueString()}, &out, http.StatusOK)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.fromResponse(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *orgResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state orgModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	notFound, d := r.client.do2xx(ctx, "Delete org failed", "delete", http.MethodDelete, orgsPath+"/"+state.ID.ValueString(), nil, nil, http.StatusNoContent)
	if notFound {
		return
	}
	resp.Diagnostics.Append(d...)
}

func (r *orgResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
