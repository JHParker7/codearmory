package provider

import (
	"context"
	"net/http"
	"net/url"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*boardResource)(nil)
	_ resource.ResourceWithConfigure   = (*boardResource)(nil)
	_ resource.ResourceWithImportState = (*boardResource)(nil)
)

const boardsPath = "/tickets/boards"

type boardResource struct{ client *Client }

func newBoardResource() resource.Resource { return &boardResource{} }

type boardModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	Color       types.String `tfsdk:"color"`
	Position    types.Int64  `tfsdk:"position"`
	CreatedBy   types.String `tfsdk:"created_by"`
	OrgID       types.String `tfsdk:"org_id"`
	CreatedAt   types.String `tfsdk:"created_at"`
	UpdatedAt   types.String `tfsdk:"updated_at"`
	OpenCount   types.Int64  `tfsdk:"open_count"`
	TotalCount  types.Int64  `tfsdk:"total_count"`
}

type boardRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Color       string `json:"color"`
	Position    int    `json:"position"`
}

type boardResponse struct {
	BoardID     string `json:"board_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Color       string `json:"color"`
	Position    int    `json:"position"`
	CreatedBy   string `json:"created_by"`
	OrgID       string `json:"org_id"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	OpenCount   int64  `json:"open_count"`
	TotalCount  int64  `json:"total_count"`
}

func (r *boardResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_board"
}

func (r *boardResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A tickets board: a kanban board that groups tickets. Status columns are seeded server-side and managed separately.",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "Board ID (UUID).", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"name":        schema.StringAttribute{Required: true, MarkdownDescription: "Board name (unique per user/org)."},
			"description": schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString(""), MarkdownDescription: "Human-readable description."},
			"color":       schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString(""), MarkdownDescription: "Display color."},
			"position":    schema.Int64Attribute{Optional: true, Computed: true, Default: int64default.StaticInt64(0), MarkdownDescription: "Sort position among boards."},
			"created_by":  schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"org_id":      schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"created_at":  schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"updated_at":  schema.StringAttribute{Computed: true},
			"open_count":  schema.Int64Attribute{Computed: true, MarkdownDescription: "Number of open tickets on the board."},
			"total_count": schema.Int64Attribute{Computed: true, MarkdownDescription: "Total tickets on the board."},
		},
	}
}

func (r *boardResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	client, diags := clientFromProviderData(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	r.client = client
}

func (m boardModel) toRequest() boardRequest {
	return boardRequest{
		Name:        m.Name.ValueString(),
		Description: m.Description.ValueString(),
		Color:       m.Color.ValueString(),
		Position:    int(m.Position.ValueInt64()),
	}
}

func (m *boardModel) fromResponse(out boardResponse) {
	m.ID = types.StringValue(out.BoardID)
	m.Name = types.StringValue(out.Name)
	m.Description = types.StringValue(out.Description)
	m.Color = types.StringValue(out.Color)
	m.Position = types.Int64Value(int64(out.Position))
	m.CreatedBy = types.StringValue(out.CreatedBy)
	m.OrgID = types.StringValue(out.OrgID)
	m.CreatedAt = types.StringValue(out.CreatedAt)
	m.UpdatedAt = types.StringValue(out.UpdatedAt)
	m.OpenCount = types.Int64Value(out.OpenCount)
	m.TotalCount = types.Int64Value(out.TotalCount)
}

func (r *boardResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan boardModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out boardResponse
	_, d := r.client.do2xx(ctx, "Create board failed", "create", http.MethodPost, boardsPath, plan.toRequest(), &out, http.StatusCreated)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.fromResponse(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *boardResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state boardModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out boardResponse
	notFound, d := r.client.do2xx(ctx, "Read board failed", "read", http.MethodGet, boardsPath+"/"+state.ID.ValueString(), nil, &out, http.StatusOK)
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

func (r *boardResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan boardModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var out boardResponse
	_, d := r.client.do2xx(ctx, "Update board failed", "update", http.MethodPut, boardsPath+"/"+plan.ID.ValueString(), plan.toRequest(), &out, http.StatusOK)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.fromResponse(out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *boardResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state boardModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Delete requires ?confirm=<exact board name>; it cascade-deletes the board's tickets.
	p := boardsPath + "/" + state.ID.ValueString() + "?confirm=" + url.QueryEscape(state.Name.ValueString())
	notFound, d := r.client.do2xx(ctx, "Delete board failed", "delete", http.MethodDelete, p, nil, nil, http.StatusNoContent)
	if notFound {
		return
	}
	resp.Diagnostics.Append(d...)
}

func (r *boardResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
