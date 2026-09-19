package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*gitRepositoryResource)(nil)
	_ resource.ResourceWithConfigure   = (*gitRepositoryResource)(nil)
	_ resource.ResourceWithImportState = (*gitRepositoryResource)(nil)
)

// git_factory is registered/routed under this service name, so every repo route is
// addressed through the gateway as /codearmory_git_factory/repos[/{id}].
const gitFactoryReposPath = "/codearmory_git_factory/repos"

type gitRepositoryResource struct {
	client *Client
}

func newGitRepositoryResource() resource.Resource { return &gitRepositoryResource{} }

type gitRepositoryModel struct {
	ID            types.String `tfsdk:"id"`
	Name          types.String `tfsdk:"name"`
	Description   types.String `tfsdk:"description"`
	Visibility    types.String `tfsdk:"visibility"`
	OrgRepo       types.Bool   `tfsdk:"org_repo"`
	Namespace     types.String `tfsdk:"namespace"`
	DefaultBranch types.String `tfsdk:"default_branch"`
	HTTPURL       types.String `tfsdk:"http_url"`
}

// createRepoRequest is the POST body. org_repo decides whether the repo lands in the
// caller's org namespace or their personal one, and so is fixed at creation.
type createRepoRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	OrgRepo     bool   `json:"org_repo"`
	Visibility  string `json:"visibility"`
}

// updateRepoRequest is the PATCH body — a rename / description / visibility change.
type updateRepoRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Visibility  string `json:"visibility"`
}

type repoResponse struct {
	ID            string `json:"id"`
	Namespace     string `json:"namespace"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	DefaultBranch string `json:"default_branch"`
	Visibility    string `json:"visibility"`
	HTTPURL       string `json:"http_url"`
}

func (r *gitRepositoryResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_git_repository"
}

func (r *gitRepositoryResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A git repository hosted by the codearmory git_factory service. Manages the repo record (name, description, visibility); the git objects are pushed over the returned `http_url`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Repository ID (UUID) — stable across renames.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Repository name (the clone-URL path segment). A change renames the repo in place.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(""),
				MarkdownDescription: "Human-readable description.",
			},
			"visibility": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("private"),
				MarkdownDescription: "`private` (default) or `public`. Public repos are clonable by anyone; writes always go through gatekeeper.",
			},
			"org_repo": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
				MarkdownDescription: "Create the repo in the caller's org namespace instead of their personal one. Fixed at creation.",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
			"namespace": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The resolved namespace (org name or username) forming the clone-URL path segment.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"default_branch": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The repository's default branch.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"http_url": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The Smart-HTTP clone/push URL for the repository.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *gitRepositoryResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	client, diags := clientFromProviderData(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	r.client = client
}

func (m *gitRepositoryModel) fromResponse(out repoResponse) diag.Diagnostics {
	var diags diag.Diagnostics
	m.ID = types.StringValue(out.ID)
	m.Name = types.StringValue(out.Name)
	m.Description = types.StringValue(out.Description)
	m.Visibility = types.StringValue(out.Visibility)
	m.Namespace = types.StringValue(out.Namespace)
	m.DefaultBranch = types.StringValue(out.DefaultBranch)
	m.HTTPURL = types.StringValue(out.HTTPURL)
	return diags
}

func (r *gitRepositoryResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan gitRepositoryModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := createRepoRequest{
		Name:        plan.Name.ValueString(),
		Description: plan.Description.ValueString(),
		OrgRepo:     plan.OrgRepo.ValueBool(),
		Visibility:  plan.Visibility.ValueString(),
	}
	var out repoResponse
	_, d := r.client.do2xx(ctx, "Create git repository failed", "create", http.MethodPost, gitFactoryReposPath, body, &out, http.StatusCreated)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(plan.fromResponse(out)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *gitRepositoryResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state gitRepositoryModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out repoResponse
	notFound, d := r.client.do2xx(ctx, "Read git repository failed", "read", http.MethodGet, gitFactoryReposPath+"/"+state.ID.ValueString(), nil, &out, http.StatusOK)
	if notFound {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(state.fromResponse(out)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *gitRepositoryResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan gitRepositoryModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := updateRepoRequest{
		Name:        plan.Name.ValueString(),
		Description: plan.Description.ValueString(),
		Visibility:  plan.Visibility.ValueString(),
	}
	var out repoResponse
	_, d := r.client.do2xx(ctx, "Update git repository failed", "update", http.MethodPatch, gitFactoryReposPath+"/"+plan.ID.ValueString(), body, &out, http.StatusOK)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(plan.fromResponse(out)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *gitRepositoryResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state gitRepositoryModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	notFound, d := r.client.do2xx(ctx, "Delete git repository failed", "delete", http.MethodDelete, gitFactoryReposPath+"/"+state.ID.ValueString(), nil, nil, http.StatusNoContent)
	if notFound {
		return
	}
	resp.Diagnostics.Append(d...)
}

func (r *gitRepositoryResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
