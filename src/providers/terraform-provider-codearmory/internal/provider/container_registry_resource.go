package provider

import (
	"context"
	"net/http"
	"net/url"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*containerRegistryResource)(nil)
	_ resource.ResourceWithConfigure   = (*containerRegistryResource)(nil)
	_ resource.ResourceWithImportState = (*containerRegistryResource)(nil)
)

const registriesPath = "/containers/registries"

type containerRegistryResource struct{ client *Client }

func newContainerRegistryResource() resource.Resource { return &containerRegistryResource{} }

type containerRegistryModel struct {
	ID        types.String `tfsdk:"id"`
	Name      types.String `tfsdk:"name"`
	URL       types.String `tfsdk:"url"`
	Username  types.String `tfsdk:"username"`
	Password  types.String `tfsdk:"password"`
	IsDefault types.Bool   `tfsdk:"is_default"`
	CreatedAt types.String `tfsdk:"created_at"`
	UpdatedAt types.String `tfsdk:"updated_at"`
}

// registryInput uses pointers server-side for partial updates; Terraform always sends
// the full desired state, so every field is set.
type registryInput struct {
	Name      *string `json:"name,omitempty"`
	URL       *string `json:"url,omitempty"`
	Username  *string `json:"username,omitempty"`
	Password  *string `json:"password,omitempty"`
	IsDefault *bool   `json:"is_default,omitempty"`
}

type registryResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Username  string `json:"username"`
	IsDefault bool   `json:"is_default"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func (r *containerRegistryResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_container_registry"
}

func (r *containerRegistryResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An upstream container registry the containers service proxies to, with its pull credentials. Keyed by name. `password` is write-only.",
		Attributes: map[string]schema.Attribute{
			"id":         schema.StringAttribute{Computed: true, MarkdownDescription: "Registry ID (UUID).", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"name":       schema.StringAttribute{Required: true, MarkdownDescription: "Registry name (unique; immutable key).", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"url":        schema.StringAttribute{Required: true, MarkdownDescription: "Upstream registry URL."},
			"username":   schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString(""), MarkdownDescription: "Registry username."},
			"password":   schema.StringAttribute{Optional: true, Sensitive: true, MarkdownDescription: "Registry password (write-only; never returned)."},
			"is_default": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false), MarkdownDescription: "Whether this is the default registry for pulls."},
			"created_at": schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"updated_at": schema.StringAttribute{Computed: true},
		},
	}
}

func (r *containerRegistryResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	client, diags := clientFromProviderData(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	r.client = client
}

func (m containerRegistryModel) toInput() registryInput {
	name := m.Name.ValueString()
	u := m.URL.ValueString()
	user := m.Username.ValueString()
	pass := m.Password.ValueString()
	def := m.IsDefault.ValueBool()
	return registryInput{Name: &name, URL: &u, Username: &user, Password: &pass, IsDefault: &def}
}

// fromResponse refreshes server-owned fields; password is write-only and kept from state.
func (m *containerRegistryModel) fromResponse(out registryResponse) {
	m.ID = types.StringValue(out.ID)
	m.Name = types.StringValue(out.Name)
	m.URL = types.StringValue(out.URL)
	m.Username = types.StringValue(out.Username)
	m.IsDefault = types.BoolValue(out.IsDefault)
	m.CreatedAt = types.StringValue(out.CreatedAt)
	m.UpdatedAt = types.StringValue(out.UpdatedAt)
}

func (r *containerRegistryResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan containerRegistryModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out registryResponse
	_, d := r.client.do2xx(ctx, "Create container registry failed", "create", http.MethodPost, registriesPath, plan.toInput(), &out, http.StatusCreated)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.fromResponse(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *containerRegistryResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state containerRegistryModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out registryResponse
	notFound, d := r.client.do2xx(ctx, "Read container registry failed", "read", http.MethodGet, registriesPath+"/"+url.PathEscape(state.Name.ValueString()), nil, &out, http.StatusOK)
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

func (r *containerRegistryResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan containerRegistryModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out registryResponse
	_, d := r.client.do2xx(ctx, "Update container registry failed", "update", http.MethodPatch, registriesPath+"/"+url.PathEscape(plan.Name.ValueString()), plan.toInput(), &out, http.StatusOK)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.fromResponse(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *containerRegistryResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state containerRegistryModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	notFound, d := r.client.do2xx(ctx, "Delete container registry failed", "delete", http.MethodDelete, registriesPath+"/"+url.PathEscape(state.Name.ValueString()), nil, nil, http.StatusNoContent)
	if notFound {
		return
	}
	resp.Diagnostics.Append(d...)
}

func (r *containerRegistryResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("name"), req, resp)
}
