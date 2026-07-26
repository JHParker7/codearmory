package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*orgServiceResource)(nil)
	_ resource.ResourceWithConfigure   = (*orgServiceResource)(nil)
	_ resource.ResourceWithImportState = (*orgServiceResource)(nil)
)

const servicesPath = "/builder/services"

type orgServiceResource struct{ client *Client }

func newOrgServiceResource() resource.Resource { return &orgServiceResource{} }

type orgServiceModel struct {
	ID               types.String `tfsdk:"id"`
	Service          types.String `tfsdk:"service"`
	Enabled          types.Bool   `tfsdk:"enabled"`
	Kind             types.String `tfsdk:"kind"`
	Config           types.String `tfsdk:"config"`
	Image            types.String `tfsdk:"image"`
	Port             types.Int64  `tfsdk:"port"`
	Description      types.String `tfsdk:"description"`
	DBURL            types.String `tfsdk:"db_url"`
	MaintenanceDBURL types.String `tfsdk:"maintenance_db_url"`
	Secrets          types.Map    `tfsdk:"secrets"`
	Source           types.String `tfsdk:"source"`
	DBConfigured     types.Bool   `tfsdk:"db_configured"`
	DBHost           types.String `tfsdk:"db_host"`
}

type setServiceRequest struct {
	Enabled          *bool             `json:"enabled,omitempty"`
	Kind             string            `json:"kind,omitempty"`
	Config           map[string]any    `json:"config,omitempty"`
	Image            string            `json:"image,omitempty"`
	Port             int               `json:"port,omitempty"`
	Description      string            `json:"description,omitempty"`
	DBURL            string            `json:"db_url,omitempty"`
	MaintenanceDBURL string            `json:"maintenance_db_url,omitempty"`
	Secrets          map[string]string `json:"secrets,omitempty"`
}

type serviceViewResponse struct {
	Service      string `json:"service"`
	Enabled      bool   `json:"enabled"`
	Kind         string `json:"kind"`
	Source       string `json:"source"`
	Image        string `json:"image"`
	Port         int    `json:"port"`
	Description  string `json:"description"`
	DBConfigured bool   `json:"db_configured"`
	DBHost       string `json:"db_host"`
}

func (r *orgServiceResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org_service"
}

func (r *orgServiceResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A builder-managed service: enables and configures a non-core platform service for the instance. Keyed by service name (upsert semantics). Write-only `db_url`/`maintenance_db_url`/`secrets` are never returned.",
		Attributes: map[string]schema.Attribute{
			"id":                 schema.StringAttribute{Computed: true, MarkdownDescription: "Same as `service` (the resource key).", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"service":            schema.StringAttribute{Required: true, MarkdownDescription: "Service name (immutable key).", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"enabled":            schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), MarkdownDescription: "Whether the service is enabled."},
			"kind":               schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "`platform` (default) or `custom`."},
			"config":             schema.StringAttribute{Optional: true, MarkdownDescription: "Service config as a JSON object string (replaced wholesale on each apply)."},
			"image":              schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Container image (required when kind=custom)."},
			"port":               schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: "Service port (required when kind=custom)."},
			"description":        schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Human-readable description."},
			"db_url":             schema.StringAttribute{Optional: true, Sensitive: true, MarkdownDescription: "Database URL (write-only; only a redacted host is echoed via db_host)."},
			"maintenance_db_url": schema.StringAttribute{Optional: true, Sensitive: true, MarkdownDescription: "One-shot maintenance DB URL for provisioning (write-only; never stored/returned)."},
			"secrets":            schema.MapAttribute{ElementType: types.StringType, Optional: true, Sensitive: true, MarkdownDescription: "Service secrets (write-only; never returned)."},
			"source":             schema.StringAttribute{Computed: true, MarkdownDescription: "Where the effective config came from (core|default|override|custom|registry|catalog)."},
			"db_configured":      schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether a db_url is configured."},
			"db_host":            schema.StringAttribute{Computed: true, MarkdownDescription: "Redacted DB host, when a db_url is set."},
		},
	}
}

func (r *orgServiceResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	client, diags := clientFromProviderData(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	r.client = client
}

func (m orgServiceModel) toRequest(ctx context.Context) (setServiceRequest, diag.Diagnostics) {
	var diags diag.Diagnostics
	enabled := m.Enabled.ValueBool()
	body := setServiceRequest{
		Enabled:          &enabled,
		Kind:             m.Kind.ValueString(),
		Image:            m.Image.ValueString(),
		Port:             int(m.Port.ValueInt64()),
		Description:      m.Description.ValueString(),
		DBURL:            m.DBURL.ValueString(),
		MaintenanceDBURL: m.MaintenanceDBURL.ValueString(),
	}
	if !m.Config.IsNull() && !m.Config.IsUnknown() && m.Config.ValueString() != "" {
		var cfg map[string]any
		if err := json.Unmarshal([]byte(m.Config.ValueString()), &cfg); err != nil {
			diags.AddError("Invalid config JSON", err.Error())
			return body, diags
		}
		body.Config = cfg
	}
	if !m.Secrets.IsNull() && !m.Secrets.IsUnknown() {
		secrets := map[string]string{}
		diags.Append(m.Secrets.ElementsAs(ctx, &secrets, false)...)
		body.Secrets = secrets
	}
	return body, diags
}

// fromResponse refreshes server-owned fields; config/secrets/db_url/maintenance_db_url
// are kept from state (config is user-authored JSON; the rest are write-only).
func (m *orgServiceModel) fromResponse(out serviceViewResponse) {
	m.ID = types.StringValue(out.Service)
	m.Service = types.StringValue(out.Service)
	m.Enabled = types.BoolValue(out.Enabled)
	m.Kind = types.StringValue(out.Kind)
	m.Image = types.StringValue(out.Image)
	m.Port = types.Int64Value(int64(out.Port))
	m.Description = types.StringValue(out.Description)
	m.Source = types.StringValue(out.Source)
	m.DBConfigured = types.BoolValue(out.DBConfigured)
	m.DBHost = types.StringValue(out.DBHost)
}

func (r *orgServiceResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan orgServiceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body, d := plan.toRequest(ctx)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out serviceViewResponse
	_, d = r.client.do2xx(ctx, "Configure org service failed", "create", http.MethodPut, servicesPath+"/"+url.PathEscape(plan.Service.ValueString()), body, &out, http.StatusOK)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.fromResponse(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *orgServiceResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state orgServiceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out serviceViewResponse
	notFound, d := r.client.do2xx(ctx, "Read org service failed", "read", http.MethodGet, servicesPath+"/"+url.PathEscape(state.Service.ValueString()), nil, &out, http.StatusOK)
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

func (r *orgServiceResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan orgServiceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body, d := plan.toRequest(ctx)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out serviceViewResponse
	_, d = r.client.do2xx(ctx, "Configure org service failed", "update", http.MethodPut, servicesPath+"/"+url.PathEscape(plan.Service.ValueString()), body, &out, http.StatusOK)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.fromResponse(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *orgServiceResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state orgServiceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	notFound, d := r.client.do2xx(ctx, "Delete org service failed", "delete", http.MethodDelete, servicesPath+"/"+url.PathEscape(state.Service.ValueString()), nil, nil, http.StatusNoContent)
	if notFound {
		return
	}
	resp.Diagnostics.Append(d...)
}

func (r *orgServiceResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Import by service name.
	resource.ImportStatePassthroughID(ctx, path.Root("service"), req, resp)
}
