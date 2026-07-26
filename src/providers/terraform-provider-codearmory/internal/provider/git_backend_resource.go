package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
)

var (
	_ resource.Resource                = (*gitBackendResource)(nil)
	_ resource.ResourceWithConfigure   = (*gitBackendResource)(nil)
	_ resource.ResourceWithImportState = (*gitBackendResource)(nil)
)

const gitBackendsPath = "/git_connector/backends"

type gitBackendResource struct{ client *Client }

func newGitBackendResource() resource.Resource { return &gitBackendResource{} }

type gitBackendModel struct {
	ID           types.String `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	Type         types.String `tfsdk:"type"`
	BaseURL      types.String `tfsdk:"base_url"`
	PreferMirror types.Bool   `tfsdk:"prefer_mirror"`
	Auth         types.Object `tfsdk:"auth"`
	Host         types.String `tfsdk:"host"`
	AuthMode     types.String `tfsdk:"auth_mode"`
	CreatedAt    types.String `tfsdk:"created_at"`
	UpdatedAt    types.String `tfsdk:"updated_at"`
}

type gitBackendAuthModel struct {
	Mode           types.String `tfsdk:"mode"`
	AppID          types.Int64  `tfsdk:"app_id"`
	InstallationID types.Int64  `tfsdk:"installation_id"`
	PrivateKey     types.String `tfsdk:"private_key"`
	Token          types.String `tfsdk:"token"`
	Username       types.String `tfsdk:"username"`
	Password       types.String `tfsdk:"password"`
	RefreshToken   types.String `tfsdk:"refresh_token"`
	ClientID       types.String `tfsdk:"client_id"`
	ClientSecret   types.String `tfsdk:"client_secret"`
	AdminToken     types.String `tfsdk:"admin_token"`
}

type gitBackendAuthRequest struct {
	Mode           string `json:"mode"`
	AppID          int64  `json:"app_id,omitempty"`
	InstallationID int64  `json:"installation_id,omitempty"`
	PrivateKey     string `json:"private_key,omitempty"`
	Token          string `json:"token,omitempty"`
	Username       string `json:"username,omitempty"`
	Password       string `json:"password,omitempty"`
	RefreshToken   string `json:"refresh_token,omitempty"`
	ClientID       string `json:"client_id,omitempty"`
	ClientSecret   string `json:"client_secret,omitempty"`
	AdminToken     string `json:"admin_token,omitempty"`
}

type createBackendRequest struct {
	Name         string                `json:"name"`
	Type         string                `json:"type"`
	BaseURL      string                `json:"base_url"`
	PreferMirror bool                  `json:"prefer_mirror"`
	Auth         gitBackendAuthRequest `json:"auth"`
}

type updateBackendRequest struct {
	BaseURL      string                 `json:"base_url,omitempty"`
	Auth         *gitBackendAuthRequest `json:"auth,omitempty"`
	PreferMirror *bool                  `json:"prefer_mirror,omitempty"`
}

type backendResponse struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	BaseURL      string `json:"base_url"`
	Host         string `json:"host"`
	AuthMode     string `json:"auth_mode"`
	PreferMirror bool   `json:"prefer_mirror"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

func (r *gitBackendResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_git_backend"
}

func (r *gitBackendResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A git_connector backend: credentials for a git host (GitHub/GitLab/Forgejo/generic) that the platform mints short-lived clone tokens against. The `auth` block is write-only.",
		Attributes: map[string]schema.Attribute{
			"id":            schema.StringAttribute{Computed: true, MarkdownDescription: "Backend ID (UUID).", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"name":          schema.StringAttribute{Required: true, MarkdownDescription: "Backend name (immutable).", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"type":          schema.StringAttribute{Required: true, MarkdownDescription: "Backend type: `github`, `gitlab`, `forgejo`, or `generic` (immutable).", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"base_url":      schema.StringAttribute{Required: true, MarkdownDescription: "Base URL of the git host, e.g. `https://github.com`."},
			"prefer_mirror": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false), MarkdownDescription: "Prefer the pull-through mirror cache when cloning."},
			"host":          schema.StringAttribute{Computed: true, MarkdownDescription: "Host parsed from base_url (unique per owner).", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"auth_mode":     schema.StringAttribute{Computed: true, MarkdownDescription: "The resolved auth mode echoed by the API.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"created_at":    schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"updated_at":    schema.StringAttribute{Computed: true},
			"auth": schema.SingleNestedAttribute{
				Required:            true,
				MarkdownDescription: "Credentials for the backend. Write-only — never returned by the API. Which fields apply depends on `mode`.",
				Attributes: map[string]schema.Attribute{
					"mode":            schema.StringAttribute{Required: true, MarkdownDescription: "Auth mode. github: `app`|`pat`; gitlab: `token`|`oauth`; forgejo: `admin`; generic: `basic`."},
					"app_id":          schema.Int64Attribute{Optional: true, MarkdownDescription: "GitHub App ID (github app mode)."},
					"installation_id": schema.Int64Attribute{Optional: true, MarkdownDescription: "GitHub App installation ID (github app mode)."},
					"private_key":     schema.StringAttribute{Optional: true, Sensitive: true, MarkdownDescription: "GitHub App private key (github app mode)."},
					"token":           schema.StringAttribute{Optional: true, Sensitive: true, MarkdownDescription: "Access token (token/pat modes)."},
					"username":        schema.StringAttribute{Optional: true, MarkdownDescription: "Username (token modes)."},
					"password":        schema.StringAttribute{Optional: true, Sensitive: true, MarkdownDescription: "Password (generic basic mode)."},
					"refresh_token":   schema.StringAttribute{Optional: true, Sensitive: true, MarkdownDescription: "OAuth refresh token (gitlab oauth mode)."},
					"client_id":       schema.StringAttribute{Optional: true, MarkdownDescription: "OAuth client ID (gitlab oauth mode)."},
					"client_secret":   schema.StringAttribute{Optional: true, Sensitive: true, MarkdownDescription: "OAuth client secret (gitlab oauth mode)."},
					"admin_token":     schema.StringAttribute{Optional: true, Sensitive: true, MarkdownDescription: "Admin token (forgejo admin mode)."},
				},
			},
		},
	}
}

func (r *gitBackendResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	client, diags := clientFromProviderData(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	r.client = client
}

func (m gitBackendModel) authRequest(ctx context.Context) (gitBackendAuthRequest, diag.Diagnostics) {
	var a gitBackendAuthModel
	diags := m.Auth.As(ctx, &a, basetypes.ObjectAsOptions{})
	return gitBackendAuthRequest{
		Mode:           a.Mode.ValueString(),
		AppID:          a.AppID.ValueInt64(),
		InstallationID: a.InstallationID.ValueInt64(),
		PrivateKey:     a.PrivateKey.ValueString(),
		Token:          a.Token.ValueString(),
		Username:       a.Username.ValueString(),
		Password:       a.Password.ValueString(),
		RefreshToken:   a.RefreshToken.ValueString(),
		ClientID:       a.ClientID.ValueString(),
		ClientSecret:   a.ClientSecret.ValueString(),
		AdminToken:     a.AdminToken.ValueString(),
	}, diags
}

// fromResponse refreshes server-owned fields; auth is write-only and kept from state.
func (m *gitBackendModel) fromResponse(out backendResponse) {
	m.ID = types.StringValue(out.ID)
	m.Name = types.StringValue(out.Name)
	m.Type = types.StringValue(out.Type)
	m.BaseURL = types.StringValue(out.BaseURL)
	m.PreferMirror = types.BoolValue(out.PreferMirror)
	m.Host = types.StringValue(out.Host)
	m.AuthMode = types.StringValue(out.AuthMode)
	m.CreatedAt = types.StringValue(out.CreatedAt)
	m.UpdatedAt = types.StringValue(out.UpdatedAt)
}

func (r *gitBackendResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan gitBackendModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	auth, d := plan.authRequest(ctx)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := createBackendRequest{
		Name:         plan.Name.ValueString(),
		Type:         plan.Type.ValueString(),
		BaseURL:      plan.BaseURL.ValueString(),
		PreferMirror: plan.PreferMirror.ValueBool(),
		Auth:         auth,
	}
	var out backendResponse
	_, d = r.client.do2xx(ctx, "Create git backend failed", "create", http.MethodPost, gitBackendsPath, body, &out, http.StatusCreated)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.fromResponse(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *gitBackendResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state gitBackendModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out backendResponse
	notFound, d := r.client.do2xx(ctx, "Read git backend failed", "read", http.MethodGet, gitBackendsPath+"/"+state.ID.ValueString(), nil, &out, http.StatusOK)
	if notFound {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	state.fromResponse(out) // auth kept from state (write-only)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *gitBackendResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan gitBackendModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	auth, d := plan.authRequest(ctx)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	prefer := plan.PreferMirror.ValueBool()
	body := updateBackendRequest{BaseURL: plan.BaseURL.ValueString(), Auth: &auth, PreferMirror: &prefer}
	var out backendResponse
	_, d = r.client.do2xx(ctx, "Update git backend failed", "update", http.MethodPut, gitBackendsPath+"/"+plan.ID.ValueString(), body, &out, http.StatusOK)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.fromResponse(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *gitBackendResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state gitBackendModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	notFound, d := r.client.do2xx(ctx, "Delete git backend failed", "delete", http.MethodDelete, gitBackendsPath+"/"+state.ID.ValueString(), nil, nil, http.StatusNoContent)
	if notFound {
		return
	}
	resp.Diagnostics.Append(d...)
}

func (r *gitBackendResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
