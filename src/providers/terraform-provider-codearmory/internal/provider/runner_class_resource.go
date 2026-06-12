package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*runnerClassResource)(nil)
	_ resource.ResourceWithConfigure   = (*runnerClassResource)(nil)
	_ resource.ResourceWithImportState = (*runnerClassResource)(nil)
)

type runnerClassResource struct {
	client *Client
}

func newRunnerClassResource() resource.Resource { return &runnerClassResource{} }

type runnerClassModel struct {
	Name          types.String `tfsdk:"name"`
	MemoryMB      types.Int64  `tfsdk:"memory_mb"`
	CPUMillicores types.Int64  `tfsdk:"cpu_millicores"`
	PidsLimit     types.Int64  `tfsdk:"pids_limit"`
	TmpfsMB       types.Int64  `tfsdk:"tmpfs_mb"`
	Enabled       types.Bool   `tfsdk:"enabled"`
}

type runnerClassAPI struct {
	Name          string `json:"name"`
	MemoryMB      int64  `json:"memory_mb"`
	CPUMillicores int64  `json:"cpu_millicores"`
	PidsLimit     int64  `json:"pids_limit"`
	TmpfsMB       int64  `json:"tmpfs_mb"`
	Enabled       bool   `json:"enabled"`
}

func (r *runnerClassResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_runner_class"
}

func (r *runnerClassResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A Forge runner class — a named resource profile (memory/CPU/pids/tmpfs) selectable for sandboxed executions.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Unique runner class name. Changing it forces replacement.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"memory_mb": schema.Int64Attribute{
				Required:            true,
				MarkdownDescription: "Memory limit in MiB (minimum 64, enforced server-side).",
			},
			"cpu_millicores": schema.Int64Attribute{
				Required:            true,
				MarkdownDescription: "CPU limit in millicores (minimum 100, enforced server-side).",
			},
			"pids_limit": schema.Int64Attribute{
				Optional:            true,
				Computed:            true,
				Default:             int64default.StaticInt64(64),
				MarkdownDescription: "Maximum number of PIDs (minimum 8). Defaults to 64.",
			},
			"tmpfs_mb": schema.Int64Attribute{
				Optional:            true,
				Computed:            true,
				Default:             int64default.StaticInt64(64),
				MarkdownDescription: "tmpfs size in MiB (minimum 16). Defaults to 64.",
			},
			"enabled": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
				MarkdownDescription: "Whether the runner class can be selected. Defaults to true.",
			},
		},
	}
}

func (r *runnerClassResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("expected *Client, got %T", req.ProviderData))
		return
	}
	r.client = client
}

func (m runnerClassModel) toAPI() runnerClassAPI {
	return runnerClassAPI{
		Name:          m.Name.ValueString(),
		MemoryMB:      m.MemoryMB.ValueInt64(),
		CPUMillicores: m.CPUMillicores.ValueInt64(),
		PidsLimit:     m.PidsLimit.ValueInt64(),
		TmpfsMB:       m.TmpfsMB.ValueInt64(),
		Enabled:       m.Enabled.ValueBool(),
	}
}

func (m *runnerClassModel) fromAPI(a runnerClassAPI) {
	m.Name = types.StringValue(a.Name)
	m.MemoryMB = types.Int64Value(a.MemoryMB)
	m.CPUMillicores = types.Int64Value(a.CPUMillicores)
	m.PidsLimit = types.Int64Value(a.PidsLimit)
	m.TmpfsMB = types.Int64Value(a.TmpfsMB)
	m.Enabled = types.BoolValue(a.Enabled)
}

func (r *runnerClassResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan runnerClassModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	status, body, err := r.client.do(ctx, http.MethodPost, "/forge/runner-classes", plan.toAPI())
	if err != nil {
		resp.Diagnostics.AddError("Create runner class failed", err.Error())
		return
	}
	if status != http.StatusCreated {
		resp.Diagnostics.AddError("Create runner class failed", apiError("create", status, body).Error())
		return
	}
	var out runnerClassAPI
	if err := json.Unmarshal(body, &out); err != nil {
		resp.Diagnostics.AddError("Decode response failed", err.Error())
		return
	}
	plan.fromAPI(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *runnerClassResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state runnerClassModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	status, body, err := r.client.do(ctx, http.MethodGet, "/forge/runner-classes/"+state.Name.ValueString(), nil)
	if err != nil {
		resp.Diagnostics.AddError("Read runner class failed", err.Error())
		return
	}
	if status == http.StatusNotFound {
		resp.State.RemoveResource(ctx)
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
	state.fromAPI(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *runnerClassResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan runnerClassModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	status, body, err := r.client.do(ctx, http.MethodPut, "/forge/runner-classes/"+plan.Name.ValueString(), plan.toAPI())
	if err != nil {
		resp.Diagnostics.AddError("Update runner class failed", err.Error())
		return
	}
	if status != http.StatusOK {
		resp.Diagnostics.AddError("Update runner class failed", apiError("update", status, body).Error())
		return
	}
	var out runnerClassAPI
	if err := json.Unmarshal(body, &out); err != nil {
		resp.Diagnostics.AddError("Decode response failed", err.Error())
		return
	}
	plan.fromAPI(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *runnerClassResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state runnerClassModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	status, body, err := r.client.do(ctx, http.MethodDelete, "/forge/runner-classes/"+state.Name.ValueString(), nil)
	if err != nil {
		resp.Diagnostics.AddError("Delete runner class failed", err.Error())
		return
	}
	if status != http.StatusNoContent && status != http.StatusNotFound {
		resp.Diagnostics.AddError("Delete runner class failed", apiError("delete", status, body).Error())
	}
}

func (r *runnerClassResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Import by runner class name: `tofu import codearmory_runner_class.x standard`.
	resource.ImportStatePassthroughID(ctx, path.Root("name"), req, resp)
}
