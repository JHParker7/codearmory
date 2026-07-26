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
	_ resource.Resource                = (*teamResource)(nil)
	_ resource.ResourceWithConfigure   = (*teamResource)(nil)
	_ resource.ResourceWithImportState = (*teamResource)(nil)
)

const teamsPath = "/gatekeeper/teams"

type teamResource struct{ client *Client }

func newTeamResource() resource.Resource { return &teamResource{} }

type teamModel struct {
	ID        types.String `tfsdk:"id"`
	TeamName  types.String `tfsdk:"team_name"`
	RoleID    types.String `tfsdk:"role_id"`
	OwnerID   types.String `tfsdk:"owner_id"`
	OrgID     types.String `tfsdk:"org_id"`
	Active    types.Bool   `tfsdk:"active"`
	CreatedAt types.String `tfsdk:"created_at"`
	UpdatedAt types.String `tfsdk:"updated_at"`
}

// teamCreateRequest — role_id is a pointer so it can be omitted on create.
type teamCreateRequest struct {
	TeamName string  `json:"team_name"`
	RoleID   *string `json:"role_id,omitempty"`
}

type teamUpdateRequest struct {
	TeamName string `json:"team_name"`
}

type teamResponse struct {
	TeamID    string  `json:"team_id"`
	TeamName  string  `json:"team_name"`
	RoleID    *string `json:"role_id"`
	OwnerID   string  `json:"owner_id"`
	OrgID     *string `json:"org_id"`
	Active    bool    `json:"active"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

func (r *teamResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_team"
}

func (r *teamResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A gatekeeper team: a named group of users, optionally granted a role.",
		Attributes: map[string]schema.Attribute{
			"id":         schema.StringAttribute{Computed: true, MarkdownDescription: "Team ID (UUID).", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"team_name":  schema.StringAttribute{Required: true, MarkdownDescription: "Team name."},
			"role_id":    schema.StringAttribute{Optional: true, MarkdownDescription: "Role granted to the team's members (set only at creation)."},
			"owner_id":   schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"org_id":     schema.StringAttribute{Computed: true, MarkdownDescription: "Owning org ID, if any.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"active":     schema.BoolAttribute{Computed: true},
			"created_at": schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"updated_at": schema.StringAttribute{Computed: true},
		},
	}
}

func (r *teamResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	client, diags := clientFromProviderData(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	r.client = client
}

func nullableString(p *string) types.String {
	if p == nil {
		return types.StringNull()
	}
	return types.StringValue(*p)
}

func (m *teamModel) fromResponse(out teamResponse) {
	m.ID = types.StringValue(out.TeamID)
	m.TeamName = types.StringValue(out.TeamName)
	m.RoleID = nullableString(out.RoleID)
	m.OwnerID = types.StringValue(out.OwnerID)
	m.OrgID = nullableString(out.OrgID)
	m.Active = types.BoolValue(out.Active)
	m.CreatedAt = types.StringValue(out.CreatedAt)
	m.UpdatedAt = types.StringValue(out.UpdatedAt)
}

func (r *teamResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan teamModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := teamCreateRequest{TeamName: plan.TeamName.ValueString()}
	if !plan.RoleID.IsNull() && !plan.RoleID.IsUnknown() {
		v := plan.RoleID.ValueString()
		body.RoleID = &v
	}
	var out teamResponse
	_, d := r.client.do2xx(ctx, "Create team failed", "create", http.MethodPost, teamsPath, body, &out, http.StatusCreated)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.fromResponse(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *teamResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state teamModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out teamResponse
	notFound, d := r.client.do2xx(ctx, "Read team failed", "read", http.MethodGet, teamsPath+"/"+state.ID.ValueString(), nil, &out, http.StatusOK)
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

func (r *teamResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan teamModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out teamResponse
	_, d := r.client.do2xx(ctx, "Update team failed", "update", http.MethodPut, teamsPath+"/"+plan.ID.ValueString(), teamUpdateRequest{TeamName: plan.TeamName.ValueString()}, &out, http.StatusOK)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.fromResponse(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *teamResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state teamModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	notFound, d := r.client.do2xx(ctx, "Delete team failed", "delete", http.MethodDelete, teamsPath+"/"+state.ID.ValueString(), nil, nil, http.StatusNoContent)
	if notFound {
		return
	}
	resp.Diagnostics.Append(d...)
}

func (r *teamResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
