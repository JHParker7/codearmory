package provider

import (
	"context"
	"encoding/json"
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
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("expected *Client, got %T", req.ProviderData))
		return
	}
	d.client = client
}

func (d *runnerClassDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg runnerClassModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	status, body, err := d.client.do(ctx, http.MethodGet, "/forge/runner-classes/"+cfg.Name.ValueString(), nil)
	if err != nil {
		resp.Diagnostics.AddError("Read runner class failed", err.Error())
		return
	}
	if status == http.StatusNotFound {
		resp.Diagnostics.AddError("Runner class not found", fmt.Sprintf("no runner class named %q", cfg.Name.ValueString()))
		return
	}
	if status != http.StatusOK {
		resp.Diagnostics.AddError("Read runner class failed", apiError("read", status, body).Error())
		return
	}
	var out runnerClassAPI
	if err := json.Unmarshal(body, &out); err != nil {
		resp.Diagnostics.AddError("Decode response failed", err.Error())
		return
	}
	cfg.fromAPI(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &cfg)...)
}
