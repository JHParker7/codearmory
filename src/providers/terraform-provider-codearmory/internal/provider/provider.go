package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ provider.Provider = (*codearmoryProvider)(nil)

type codearmoryProvider struct {
	version string
}

// New returns the provider factory used by providerserver.Serve.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &codearmoryProvider{version: version}
	}
}

// providerModel mirrors the provider configuration block.
type providerModel struct {
	Endpoint     types.String `tfsdk:"endpoint"`
	Token        types.String `tfsdk:"token"`
	ClientID     types.String `tfsdk:"client_id"`
	ClientSecret types.String `tfsdk:"client_secret"`
}

func (p *codearmoryProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "codearmory"
	resp.Version = p.version
}

func (p *codearmoryProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Configure a codearmory platform through its Conductor gateway. " +
			"Authenticate with a pre-issued bearer `token`, or with an OAuth `client_id`/`client_secret` " +
			"(client_credentials grant).",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Conductor base URL, e.g. `https://conductor.example.com`. Falls back to `CODEARMORY_URL`.",
			},
			"token": schema.StringAttribute{
				Optional:            true,
				Sensitive:           true,
				MarkdownDescription: "A pre-issued bearer token. Takes precedence over `client_id`/`client_secret`. Falls back to `CODEARMORY_TOKEN`.",
			},
			"client_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "OAuth client_id for the client_credentials grant. Falls back to `CODEARMORY_CLIENT_ID`.",
			},
			"client_secret": schema.StringAttribute{
				Optional:            true,
				Sensitive:           true,
				MarkdownDescription: "OAuth client_secret for the client_credentials grant. Falls back to `CODEARMORY_CLIENT_SECRET`.",
			},
		},
	}
}

func (p *codearmoryProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	endpoint := firstNonEmpty(cfg.Endpoint.ValueString(), os.Getenv("CODEARMORY_URL"))
	token := firstNonEmpty(cfg.Token.ValueString(), os.Getenv("CODEARMORY_TOKEN"))
	clientID := firstNonEmpty(cfg.ClientID.ValueString(), os.Getenv("CODEARMORY_CLIENT_ID"))
	clientSecret := firstNonEmpty(cfg.ClientSecret.ValueString(), os.Getenv("CODEARMORY_CLIENT_SECRET"))

	if endpoint == "" {
		resp.Diagnostics.AddAttributeError(
			path.Root("endpoint"),
			"Missing Conductor endpoint",
			"Set the provider `endpoint` argument or the CODEARMORY_URL environment variable.",
		)
		return
	}

	if token == "" {
		if clientID == "" || clientSecret == "" {
			resp.Diagnostics.AddError(
				"Missing credentials",
				"Provide either `token` (CODEARMORY_TOKEN), or both `client_id` and `client_secret` "+
					"(CODEARMORY_CLIENT_ID / CODEARMORY_CLIENT_SECRET) for the client_credentials grant.",
			)
			return
		}
		t, err := fetchClientCredentialsToken(ctx, endpoint, clientID, clientSecret)
		if err != nil {
			resp.Diagnostics.AddError("Authentication failed", err.Error())
			return
		}
		token = t
	}

	client := newClient(endpoint, token)
	resp.ResourceData = client
	resp.DataSourceData = client
}

func (p *codearmoryProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		newRunnerClassResource,
		newEventTriggerResource,
		newGitRepositoryResource,
		newPipelineResource,
		newStepResource,
		newGitBackendResource,
		newOrgResource,
		newTeamResource,
		newRoleResource,
		newSecretResource,
		newBoardResource,
		newOrgServiceResource,
		newOutpostResource,
		newContainerRegistryResource,
	}
}

func (p *codearmoryProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		newRunnerClassDataSource,
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
