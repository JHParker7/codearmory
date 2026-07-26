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
	_ resource.Resource                = (*secretResource)(nil)
	_ resource.ResourceWithConfigure   = (*secretResource)(nil)
	_ resource.ResourceWithImportState = (*secretResource)(nil)
)

const secretsPath = "/gatekeeper/secrets"

type secretResource struct{ client *Client }

func newSecretResource() resource.Resource { return &secretResource{} }

type secretModel struct {
	ID        types.String `tfsdk:"id"`
	Name      types.String `tfsdk:"name"`
	Value     types.String `tfsdk:"value"`
	OrgID     types.String `tfsdk:"org_id"`
	CreatedBy types.String `tfsdk:"created_by"`
	CreatedAt types.String `tfsdk:"created_at"`
	UpdatedAt types.String `tfsdk:"updated_at"`
}

type secretRequest struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type secretResponse struct {
	SecretID  string `json:"secret_id"`
	OrgID     string `json:"org_id"`
	Name      string `json:"name"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func (r *secretResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_secret"
}

func (r *secretResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A gatekeeper secret: a named, encrypted value scoped to the caller's active org. The value is write-only — it is never returned by the API and cannot be read back.",
		Attributes: map[string]schema.Attribute{
			"id":         schema.StringAttribute{Computed: true, MarkdownDescription: "Secret ID (UUID).", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"name":       schema.StringAttribute{Required: true, MarkdownDescription: "Secret name."},
			"value":      schema.StringAttribute{Required: true, Sensitive: true, MarkdownDescription: "Secret value (write-only; never returned by the API)."},
			"org_id":     schema.StringAttribute{Computed: true, MarkdownDescription: "Owning org ID.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"created_by": schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"created_at": schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"updated_at": schema.StringAttribute{Computed: true},
		},
	}
}

func (r *secretResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	client, diags := clientFromProviderData(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	r.client = client
}

// fromResponse refreshes the server-owned fields; value is write-only and is left
// untouched in state (the API never returns it).
func (m *secretModel) fromResponse(out secretResponse) {
	m.ID = types.StringValue(out.SecretID)
	m.Name = types.StringValue(out.Name)
	m.OrgID = types.StringValue(out.OrgID)
	m.CreatedBy = types.StringValue(out.CreatedBy)
	m.CreatedAt = types.StringValue(out.CreatedAt)
	m.UpdatedAt = types.StringValue(out.UpdatedAt)
}

func (r *secretResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan secretModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out secretResponse
	_, d := r.client.do2xx(ctx, "Create secret failed", "create", http.MethodPost, secretsPath, secretRequest{Name: plan.Name.ValueString(), Value: plan.Value.ValueString()}, &out, http.StatusCreated)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.fromResponse(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *secretResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state secretModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// There is no read-by-id route; confirm existence via the list and match on id.
	var list []secretResponse
	_, d := r.client.do2xx(ctx, "Read secret failed", "read", http.MethodGet, secretsPath, nil, &list, http.StatusOK)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	for _, s := range list {
		if s.SecretID == state.ID.ValueString() {
			state.fromResponse(s) // value stays as-is (write-only)
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			return
		}
	}
	resp.State.RemoveResource(ctx)
}

func (r *secretResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan secretModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out secretResponse
	_, d := r.client.do2xx(ctx, "Update secret failed", "update", http.MethodPut, secretsPath+"/"+plan.ID.ValueString(), secretRequest{Name: plan.Name.ValueString(), Value: plan.Value.ValueString()}, &out, http.StatusOK)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.fromResponse(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *secretResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state secretModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	notFound, d := r.client.do2xx(ctx, "Delete secret failed", "delete", http.MethodDelete, secretsPath+"/"+state.ID.ValueString(), nil, nil, http.StatusNoContent)
	if notFound {
		return
	}
	resp.Diagnostics.Append(d...)
}

func (r *secretResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
