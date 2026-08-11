package provider

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/infercrane/infercrane/internal/controlclient"
)

var _ resource.Resource = (*sloPolicyResource)(nil)
var _ resource.ResourceWithConfigure = (*sloPolicyResource)(nil)
var _ resource.ResourceWithImportState = (*sloPolicyResource)(nil)

type sloPolicyResource struct{ client *controlclient.Client }
type sloPolicyModel struct {
	ID            types.String  `tfsdk:"id"`
	Deployment    types.String  `tfsdk:"deployment"`
	MaxTTFT       types.Float64 `tfsdk:"max_ttft_p95_ms"`
	MaxLatency    types.Float64 `tfsdk:"max_latency_p95_ms"`
	MaxError      types.Float64 `tfsdk:"max_error_rate"`
	MinThroughput types.Float64 `tfsdk:"min_output_tokens_second"`
	MaxHourlyCost types.Float64 `tfsdk:"max_hourly_cost"`
}

func NewSLOPolicyResource() resource.Resource { return &sloPolicyResource{} }
func (r *sloPolicyResource) Metadata(_ context.Context, request resource.MetadataRequest, response *resource.MetadataResponse) {
	response.TypeName = request.ProviderTypeName + "_slo_policy"
}
func (r *sloPolicyResource) Schema(_ context.Context, _ resource.SchemaRequest, response *resource.SchemaResponse) {
	r.schema(response)
}
func (r *sloPolicyResource) Configure(_ context.Context, request resource.ConfigureRequest, response *resource.ConfigureResponse) {
	if request.ProviderData == nil {
		return
	}
	client, ok := request.ProviderData.(*controlclient.Client)
	if !ok {
		response.Diagnostics.Append(invalidClientDiagnostic())
		return
	}
	r.client = client
}

func (r *sloPolicyResource) schema(response *resource.SchemaResponse) {
	response.Schema = schema.Schema{Description: "Deterministic fail-closed SLO policy for evidence-based inference recommendations.", Attributes: map[string]schema.Attribute{"id": schema.StringAttribute{Computed: true}, "deployment": schema.StringAttribute{Required: true, PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}}, "max_ttft_p95_ms": schema.Float64Attribute{Optional: true}, "max_latency_p95_ms": schema.Float64Attribute{Optional: true}, "max_error_rate": schema.Float64Attribute{Optional: true}, "min_output_tokens_second": schema.Float64Attribute{Optional: true}, "max_hourly_cost": schema.Float64Attribute{Optional: true}}}
}

func (r *sloPolicyResource) body(model sloPolicyModel) map[string]any {
	out := map[string]any{}
	for key, value := range map[string]types.Float64{"max_ttft_p95_ms": model.MaxTTFT, "max_latency_p95_ms": model.MaxLatency, "max_error_rate": model.MaxError, "min_output_tokens_second": model.MinThroughput, "max_hourly_cost": model.MaxHourlyCost} {
		if !value.IsNull() && !value.IsUnknown() {
			out[key] = value.ValueFloat64()
		}
	}
	return out
}
func (r *sloPolicyResource) write(ctx context.Context, model *sloPolicyModel) error {
	var response struct {
		Policy map[string]any `json:"policy"`
	}
	if err := r.client.Do(ctx, http.MethodPut, "/deployments/"+url.PathEscape(model.Deployment.ValueString())+"/slo-policy", "", r.body(*model), &response); err != nil {
		return err
	}
	model.ID = types.StringValue(model.Deployment.ValueString())
	return nil
}
func (r *sloPolicyResource) Create(ctx context.Context, request resource.CreateRequest, response *resource.CreateResponse) {
	var model sloPolicyModel
	response.Diagnostics.Append(request.Plan.Get(ctx, &model)...)
	if response.Diagnostics.HasError() {
		return
	}
	if err := r.write(ctx, &model); err != nil {
		response.Diagnostics.AddError("Create SLO policy failed", err.Error())
		return
	}
	response.Diagnostics.Append(response.State.Set(ctx, &model)...)
}
func (r *sloPolicyResource) Read(ctx context.Context, request resource.ReadRequest, response *resource.ReadResponse) {
	var model sloPolicyModel
	response.Diagnostics.Append(request.State.Get(ctx, &model)...)
	if response.Diagnostics.HasError() {
		return
	}
	var result struct {
		Policy struct {
			MaxTTFT       *float64 `json:"max_ttft_p95_ms"`
			MaxLatency    *float64 `json:"max_latency_p95_ms"`
			MaxError      *float64 `json:"max_error_rate"`
			MinThroughput *float64 `json:"min_output_tokens_second"`
			MaxHourlyCost *float64 `json:"max_hourly_cost"`
		} `json:"policy"`
	}
	err := r.client.Do(ctx, http.MethodGet, "/deployments/"+url.PathEscape(model.Deployment.ValueString())+"/slo-policy", "", nil, &result)
	var apiErr *controlclient.APIError
	if errors.As(err, &apiErr) && apiErr.Status == 404 {
		response.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		response.Diagnostics.AddError("Read SLO policy failed", err.Error())
		return
	}
	assign := func(value *float64) types.Float64 {
		if value == nil {
			return types.Float64Null()
		}
		return types.Float64Value(*value)
	}
	model.MaxTTFT = assign(result.Policy.MaxTTFT)
	model.MaxLatency = assign(result.Policy.MaxLatency)
	model.MaxError = assign(result.Policy.MaxError)
	model.MinThroughput = assign(result.Policy.MinThroughput)
	model.MaxHourlyCost = assign(result.Policy.MaxHourlyCost)
	model.ID = types.StringValue(model.Deployment.ValueString())
	response.Diagnostics.Append(response.State.Set(ctx, &model)...)
}
func (r *sloPolicyResource) Update(ctx context.Context, request resource.UpdateRequest, response *resource.UpdateResponse) {
	var model sloPolicyModel
	response.Diagnostics.Append(request.Plan.Get(ctx, &model)...)
	if response.Diagnostics.HasError() {
		return
	}
	if err := r.write(ctx, &model); err != nil {
		response.Diagnostics.AddError("Update SLO policy failed", err.Error())
		return
	}
	response.Diagnostics.Append(response.State.Set(ctx, &model)...)
}
func (r *sloPolicyResource) Delete(ctx context.Context, request resource.DeleteRequest, response *resource.DeleteResponse) {
	var model sloPolicyModel
	response.Diagnostics.Append(request.State.Get(ctx, &model)...)
	if response.Diagnostics.HasError() {
		return
	}
	err := r.client.Do(ctx, http.MethodDelete, "/deployments/"+url.PathEscape(model.Deployment.ValueString())+"/slo-policy", "", nil, nil)
	var apiErr *controlclient.APIError
	if err != nil && !(errors.As(err, &apiErr) && apiErr.Status == 404) {
		response.Diagnostics.AddError("Delete SLO policy failed", err.Error())
	}
}
func (r *sloPolicyResource) ImportState(ctx context.Context, request resource.ImportStateRequest, response *resource.ImportStateResponse) {
	response.Diagnostics.Append(response.State.SetAttribute(ctx, path.Root("id"), request.ID)...)
	response.Diagnostics.Append(response.State.SetAttribute(ctx, path.Root("deployment"), request.ID)...)
}
