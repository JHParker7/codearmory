package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
)

var (
	_ datasource.DataSource              = (*runnerClassDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*runnerClassDataSource)(nil)
)

type runnerClassDataSource struct {
	client *Client
}

func newRunnerClassDataSource() datasource.DataSource { return &runnerClassDataSource{} }

func (d *runnerClassDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_runner_class"
}

func (d *runnerClassDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up an existing Forge runner class by name.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Runner class name to look up.",
			},
			"memory_mb":      schema.Int64Attribute{Computed: true, MarkdownDescription: "Memory limit in MiB."},
			"cpu_millicores": schema.Int64Attribute{Computed: true, MarkdownDescription: "CPU limit in millicores."},
			"pids_limit":     schema.Int64Attribute{Computed: true, MarkdownDescription: "Maximum number of PIDs."},
			"tmpfs_mb":       schema.Int64Attribute{Computed: true, MarkdownDescription: "tmpfs size in MiB."},
			"enabled":        schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the runner class is selectable."},
		},
	}
}

func (d *runnerClassDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	client, diags := clientFromProviderData(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	d.client = client
}

func (d *runnerClassDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg runnerClassModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out runnerClassAPI
	notFound, diags := d.client.do2xx(ctx, "Read runner class failed", "read", http.MethodGet, "/forge/runner-classes/"+cfg.Name.ValueString(), nil, &out, http.StatusOK)
	if notFound {
		resp.Diagnostics.AddError("Runner class not found", fmt.Sprintf("no runner class named %q", cfg.Name.ValueString()))
		return
	}
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	cfg.fromAPI(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &cfg)...)
}
