package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int32validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/infercrane/infercrane/internal/controlclient"
)

var _ resource.Resource = (*deploymentResource)(nil)
var _ resource.ResourceWithConfigure = (*deploymentResource)(nil)
var _ resource.ResourceWithImportState = (*deploymentResource)(nil)

type deploymentResource struct{ client *controlclient.Client }
type deploymentModel struct {
	ID                      types.String `tfsdk:"id"`
	Name                    types.String `tfsdk:"name"`
	Model                   types.String `tfsdk:"model"`
	Runtime                 types.String `tfsdk:"runtime"`
	Cloud                   types.String `tfsdk:"cloud"`
	ComputeMode             types.String `tfsdk:"compute_mode"`
	GPU                     types.String `tfsdk:"gpu"`
	Region                  types.String `tfsdk:"region"`
	ModelRevision           types.String `tfsdk:"model_revision"`
	MinReplicas             types.Int32  `tfsdk:"min_replicas"`
	MaxReplicas             types.Int32  `tfsdk:"max_replicas"`
	OperationTimeoutSeconds types.Int32  `tfsdk:"operation_timeout_seconds"`
	ObservedState           types.String `tfsdk:"observed_state"`
	ActiveRevisionID        types.String `tfsdk:"active_revision_id"`
	CandidateRevisionID     types.String `tfsdk:"candidate_revision_id"`
	OperationID             types.String `tfsdk:"operation_id"`
}

func NewDeploymentResource() resource.Resource { return &deploymentResource{} }
func (r *deploymentResource) Metadata(_ context.Context, request resource.MetadataRequest, response *resource.MetadataResponse) {
	response.TypeName = request.ProviderTypeName + "_deployment"
}
func (r *deploymentResource) Schema(_ context.Context, _ resource.SchemaRequest, response *resource.SchemaResponse) {
	response.Schema = schema.Schema{Description: "A logical InferCrane deployment. InferCrane, not Terraform, owns provider replicas and rollout execution.", Attributes: map[string]schema.Attribute{
		"id":                        schema.StringAttribute{Computed: true, Description: "Stable logical deployment identity."},
		"name":                      schema.StringAttribute{Required: true, Description: "Logical deployment name and import identifier.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
		"model":                     schema.StringAttribute{Required: true, Description: "Model repository identity."},
		"runtime":                   schema.StringAttribute{Optional: true, Computed: true, Description: "Qualified inference runtime. Custom OCI workload details use the API or DeploymentSpec in v0.6.", Validators: []validator.String{stringvalidator.OneOf("vllm", "sglang")}},
		"cloud":                     schema.StringAttribute{Required: true, Description: "Registered provider cloud adapter."},
		"compute_mode":              schema.StringAttribute{Optional: true, Computed: true, Description: "Provider compute mode.", Validators: []validator.String{stringvalidator.OneOf("elastic", "serverless")}},
		"gpu":                       schema.StringAttribute{Required: true, Description: "Explicit provider GPU selector."},
		"region":                    schema.StringAttribute{Optional: true, Description: "Explicit provider region where supported."},
		"model_revision":            schema.StringAttribute{Optional: true, Description: "Requested immutable or resolvable model revision."},
		"min_replicas":              schema.Int32Attribute{Optional: true, Computed: true, Validators: []validator.Int32{int32validator.AtLeast(0)}},
		"max_replicas":              schema.Int32Attribute{Optional: true, Computed: true, Validators: []validator.Int32{int32validator.AtLeast(0)}},
		"operation_timeout_seconds": schema.Int32Attribute{Optional: true, Computed: true, Description: "How long this Terraform process waits. A timeout never cancels durable work.", Validators: []validator.Int32{int32validator.Between(1, 86400)}},
		"observed_state":            schema.StringAttribute{Computed: true},
		"active_revision_id":        schema.StringAttribute{Computed: true},
		"candidate_revision_id":     schema.StringAttribute{Computed: true},
		"operation_id":              schema.StringAttribute{Computed: true, Description: "Most recent durable operation for resume and inspection."},
	}}
}
func (r *deploymentResource) Configure(_ context.Context, request resource.ConfigureRequest, response *resource.ConfigureResponse) {
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

func defaults(model *deploymentModel) {
	if model.Runtime.IsNull() || model.Runtime.IsUnknown() || model.Runtime.ValueString() == "" {
		model.Runtime = types.StringValue("vllm")
	}
	if model.ComputeMode.IsNull() || model.ComputeMode.IsUnknown() || model.ComputeMode.ValueString() == "" {
		model.ComputeMode = types.StringValue("elastic")
	}
	if model.MinReplicas.IsNull() || model.MinReplicas.IsUnknown() {
		if model.ComputeMode.ValueString() == "serverless" {
			model.MinReplicas = types.Int32Value(0)
		} else {
			model.MinReplicas = types.Int32Value(1)
		}
	}
	if model.MaxReplicas.IsNull() || model.MaxReplicas.IsUnknown() {
		model.MaxReplicas = model.MinReplicas
	}
	if model.OperationTimeoutSeconds.IsNull() || model.OperationTimeoutSeconds.IsUnknown() {
		model.OperationTimeoutSeconds = types.Int32Value(900)
	}
}
func requestFor(model deploymentModel) map[string]any {
	return map[string]any{"name": model.Name.ValueString(), "model": model.Model.ValueString(), "runtime": model.Runtime.ValueString(), "cloud": model.Cloud.ValueString(), "compute_mode": model.ComputeMode.ValueString(), "gpu": model.GPU.ValueString(), "region": model.Region.ValueString(), "model_revision": model.ModelRevision.ValueString(), "min_replicas": model.MinReplicas.ValueInt32(), "max_replicas": model.MaxReplicas.ValueInt32()}
}
func operationKey(action string, request any) string {
	encoded, _ := json.Marshal(request)
	sum := sha256.Sum256(append([]byte(action+":"), encoded...))
	return "terraform-" + action + "-" + hex.EncodeToString(sum[:16])
}
func (r *deploymentResource) wait(ctx context.Context, operationID string, timeout int32) error {
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	_, err := r.client.Wait(waitCtx, operationID)
	return err
}

func (r *deploymentResource) Create(ctx context.Context, request resource.CreateRequest, response *resource.CreateResponse) {
	if r.client == nil {
		response.Diagnostics.Append(invalidClientDiagnostic())
		return
	}
	var plan deploymentModel
	response.Diagnostics.Append(request.Plan.Get(ctx, &plan)...)
	if response.Diagnostics.HasError() {
		return
	}
	defaults(&plan)
	body := requestFor(plan)
	operation, err := r.client.CreateDeployment(ctx, body, operationKey("create", body))
	if err != nil {
		response.Diagnostics.AddError("Create deployment failed", err.Error())
		return
	}
	plan.OperationID = types.StringValue(operation.ID)
	if err = r.wait(ctx, operation.ID, plan.OperationTimeoutSeconds.ValueInt32()); err != nil {
		response.Diagnostics.AddError("Deployment operation did not complete", fmt.Sprintf("%v. Resume with: infercrane operation watch %s", err, operation.ID))
		return
	}
	if err = r.refresh(ctx, &plan); err != nil {
		response.Diagnostics.AddError("Read deployment after create failed", err.Error())
		return
	}
	response.Diagnostics.Append(response.State.Set(ctx, &plan)...)
}

func (r *deploymentResource) Read(ctx context.Context, request resource.ReadRequest, response *resource.ReadResponse) {
	if r.client == nil {
		response.Diagnostics.Append(invalidClientDiagnostic())
		return
	}
	var state deploymentModel
	response.Diagnostics.Append(request.State.Get(ctx, &state)...)
	if response.Diagnostics.HasError() {
		return
	}
	err := r.refresh(ctx, &state)
	var apiError *controlclient.APIError
	if errors.As(err, &apiError) && apiError.Status == 404 {
		response.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		response.Diagnostics.AddError("Read deployment failed", err.Error())
		return
	}
	response.Diagnostics.Append(response.State.Set(ctx, &state)...)
}

func (r *deploymentResource) Update(ctx context.Context, request resource.UpdateRequest, response *resource.UpdateResponse) {
	if r.client == nil {
		response.Diagnostics.Append(invalidClientDiagnostic())
		return
	}
	var plan deploymentModel
	response.Diagnostics.Append(request.Plan.Get(ctx, &plan)...)
	if response.Diagnostics.HasError() {
		return
	}
	defaults(&plan)
	spec := map[string]any{"model": plan.Model.ValueString(), "model_revision": plan.ModelRevision.ValueString(), "runtime": plan.Runtime.ValueString(), "routing_strategy": "round-robin", "min_replicas": plan.MinReplicas.ValueInt32(), "max_replicas": plan.MaxReplicas.ValueInt32(), "autoscaling_enabled": plan.MaxReplicas.ValueInt32() > plan.MinReplicas.ValueInt32(), "compute_mode": plan.ComputeMode.ValueString(), "cloud": plan.Cloud.ValueString(), "gpu": plan.GPU.ValueString(), "region": plan.Region.ValueString()}
	createBody := map[string]any{"spec": spec}
	operation, err := r.client.Rollout(ctx, plan.Name.ValueString(), "create", "", operationKey("candidate", createBody), createBody)
	if err != nil {
		response.Diagnostics.AddError("Create candidate failed", err.Error())
		return
	}
	plan.OperationID = types.StringValue(operation.ID)
	if err = r.wait(ctx, operation.ID, plan.OperationTimeoutSeconds.ValueInt32()); err != nil {
		response.Diagnostics.AddError("Candidate creation did not complete", err.Error())
		return
	}
	current, _, err := r.client.Deployment(ctx, plan.Name.ValueString())
	if err != nil {
		response.Diagnostics.AddError("Inspect candidate failed", err.Error())
		return
	}
	candidate := current.CandidateRevisionID
	if candidate == "" {
		response.Diagnostics.AddError("Candidate identity missing", "InferCrane completed candidate creation without exposing candidate_revision_id.")
		return
	}
	for _, transition := range []struct {
		name string
		body any
	}{{"provision", nil}, {"evaluate", map[string]any{}}, {"promote", nil}} {
		operation, err = r.client.Rollout(ctx, plan.Name.ValueString(), transition.name, candidate, operationKey(transition.name, map[string]string{"name": plan.Name.ValueString(), "revision": candidate}), transition.body)
		if err != nil {
			response.Diagnostics.AddError("Rollout transition failed", fmt.Sprintf("%s: %v", transition.name, err))
			return
		}
		plan.OperationID = types.StringValue(operation.ID)
		if err = r.wait(ctx, operation.ID, plan.OperationTimeoutSeconds.ValueInt32()); err != nil {
			response.Diagnostics.AddError("Rollout transition did not complete", fmt.Sprintf("%s: %v. Release Guard may require benchmark or request evidence; inspect with infercrane rollout inspect %s.", transition.name, err, plan.Name.ValueString()))
			return
		}
	}
	if err = r.refresh(ctx, &plan); err != nil {
		response.Diagnostics.AddError("Read deployment after update failed", err.Error())
		return
	}
	response.Diagnostics.Append(response.State.Set(ctx, &plan)...)
}

func (r *deploymentResource) Delete(ctx context.Context, request resource.DeleteRequest, response *resource.DeleteResponse) {
	if r.client == nil {
		response.Diagnostics.Append(invalidClientDiagnostic())
		return
	}
	var state deploymentModel
	response.Diagnostics.Append(request.State.Get(ctx, &state)...)
	if response.Diagnostics.HasError() {
		return
	}
	defaults(&state)
	body := map[string]string{"name": state.Name.ValueString()}
	operation, err := r.client.DeleteDeployment(ctx, state.Name.ValueString(), operationKey("delete", body))
	var apiError *controlclient.APIError
	if errors.As(err, &apiError) && apiError.Status == 404 {
		return
	}
	if err != nil {
		response.Diagnostics.AddError("Delete deployment failed", err.Error())
		return
	}
	if err = r.wait(ctx, operation.ID, state.OperationTimeoutSeconds.ValueInt32()); err != nil {
		response.Diagnostics.AddError("Deletion did not complete", fmt.Sprintf("%v. Resume with: infercrane operation watch %s", err, operation.ID))
		return
	}
}

func (r *deploymentResource) ImportState(ctx context.Context, request resource.ImportStateRequest, response *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("name"), request, response)
}

func (r *deploymentResource) refresh(ctx context.Context, model *deploymentModel) error {
	row, _, err := r.client.Deployment(ctx, model.Name.ValueString())
	if err != nil {
		return err
	}
	model.ID, model.Model, model.Runtime = types.StringValue(row.ID), types.StringValue(row.Model), types.StringValue(row.Runtime)
	model.MinReplicas, model.MaxReplicas = types.Int32Value(int32(row.MinReplicas)), types.Int32Value(int32(row.MaxReplicas))
	model.Cloud, model.ComputeMode, model.GPU = types.StringValue(row.Cloud), types.StringValue(row.ComputeMode), types.StringValue(row.GPU)
	if row.Region == "" {
		model.Region = types.StringNull()
	} else {
		model.Region = types.StringValue(row.Region)
	}
	if row.ModelRevision == "" {
		model.ModelRevision = types.StringNull()
	} else {
		model.ModelRevision = types.StringValue(row.ModelRevision)
	}
	model.ObservedState, model.ActiveRevisionID, model.CandidateRevisionID = types.StringValue(row.ObservedState), types.StringValue(row.ActiveRevisionID), types.StringValue(row.CandidateRevisionID)
	return nil
}
