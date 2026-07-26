package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*roleResource)(nil)
	_ resource.ResourceWithConfigure   = (*roleResource)(nil)
	_ resource.ResourceWithImportState = (*roleResource)(nil)
)

const rolesPath = "/gatekeeper/roles"

type roleResource struct{ client *Client }

func newRoleResource() resource.Resource { return &roleResource{} }

type roleModel struct {
	ID             types.String `tfsdk:"id"`
	Name           types.String `tfsdk:"name"`
	PermissionsIDs types.List   `tfsdk:"permissions_ids"`
	OrgID          types.String `tfsdk:"org_id"`
	OwnerID        types.String `tfsdk:"owner_id"`
	Active         types.Bool   `tfsdk:"active"`
	CreatedAt      types.String `tfsdk:"created_at"`
	UpdatedAt      types.String `tfsdk:"updated_at"`
}

type roleRequest struct {
	Name           string   `json:"name,omitempty"`
	PermissionsIDs []string `json:"permissions_ids"`
	OrgID          *string  `json:"org_id,omitempty"`
}

type roleResponse struct {
	RoleID         string   `json:"role_id"`
	Name           string   `json:"name"`
	PermissionsIDs []string `json:"permissions_ids"`
	OrgID          *string  `json:"org_id"`
	OwnerID        string   `json:"owner_id"`
	Active         bool     `json:"active"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
}

func (r *roleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_role"
}

func (r *roleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A gatekeeper role: a named set of permission IDs that can be granted to users or teams.",
		Attributes: map[string]schema.Attribute{
			"id":              schema.StringAttribute{Computed: true, MarkdownDescription: "Role ID (UUID).", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"name":            schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Role name."},
			"permissions_ids": schema.ListAttribute{ElementType: types.StringType, Required: true, MarkdownDescription: "The permission IDs this role grants."},
			"org_id":          schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Owning org ID (defaults to the caller's active org).", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"owner_id":        schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"active":          schema.BoolAttribute{Computed: true},
			"created_at":      schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"updated_at":      schema.StringAttribute{Computed: true},
		},
	}
}

func (r *roleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	client, diags := clientFromProviderData(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	r.client = client
}

func (m roleModel) toRequest(ctx context.Context) (roleRequest, diag.Diagnostics) {
	var diags diag.Diagnostics
	var perms []string
	diags.Append(m.PermissionsIDs.ElementsAs(ctx, &perms, false)...)
	body := roleRequest{Name: m.Name.ValueString(), PermissionsIDs: perms}
	if !m.OrgID.IsNull() && !m.OrgID.IsUnknown() {
		v := m.OrgID.ValueString()
		body.OrgID = &v
	}
	return body, diags
}

func (m *roleModel) fromResponse(ctx context.Context, out roleResponse) diag.Diagnostics {
	var diags diag.Diagnostics
	m.ID = types.StringValue(out.RoleID)
	m.Name = types.StringValue(out.Name)
	perms, d := types.ListValueFrom(ctx, types.StringType, out.PermissionsIDs)
	diags.Append(d...)
	m.PermissionsIDs = perms
	m.OrgID = nullableString(out.OrgID)
	m.OwnerID = types.StringValue(out.OwnerID)
	m.Active = types.BoolValue(out.Active)
	m.CreatedAt = types.StringValue(out.CreatedAt)
	m.UpdatedAt = types.StringValue(out.UpdatedAt)
	return diags
}

func (r *roleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan roleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body, d := plan.toRequest(ctx)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out roleResponse
	_, d = r.client.do2xx(ctx, "Create role failed", "create", http.MethodPost, rolesPath, body, &out, http.StatusCreated)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(plan.fromResponse(ctx, out)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *roleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state roleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out roleResponse
	notFound, d := r.client.do2xx(ctx, "Read role failed", "read", http.MethodGet, rolesPath+"/"+state.ID.ValueString(), nil, &out, http.StatusOK)
	if notFound {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(state.fromResponse(ctx, out)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *roleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan roleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body, d := plan.toRequest(ctx)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out roleResponse
	_, d = r.client.do2xx(ctx, "Update role failed", "update", http.MethodPut, rolesPath+"/"+plan.ID.ValueString(), body, &out, http.StatusOK)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(plan.fromResponse(ctx, out)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *roleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state roleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	notFound, d := r.client.do2xx(ctx, "Delete role failed", "delete", http.MethodDelete, rolesPath+"/"+state.ID.ValueString(), nil, nil, http.StatusNoContent)
	if notFound {
		return
	}
	resp.Diagnostics.Append(d...)
}

func (r *roleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
