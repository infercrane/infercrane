package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/infercrane/infercrane/internal/controlclient"
)

type ProviderFactory = provider.Provider

var _ provider.Provider = (*inferCraneProvider)(nil)

type inferCraneProvider struct{ version string }
type providerModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	APIKey   types.String `tfsdk:"api_key"`
}

func New(version string) provider.Provider { return &inferCraneProvider{version: version} }
func (p *inferCraneProvider) Metadata(_ context.Context, _ provider.MetadataRequest, response *provider.MetadataResponse) {
	response.TypeName = "infercrane"
	response.Version = p.version
}
func (p *inferCraneProvider) Schema(_ context.Context, _ provider.SchemaRequest, response *provider.SchemaResponse) {
	response.Schema = schema.Schema{Description: "Manage logical InferCrane deployments through durable control-plane operations.", Attributes: map[string]schema.Attribute{
		"endpoint": schema.StringAttribute{Optional: true, Description: "InferCrane control-plane URL. Defaults to INFERCRANE_CONTROL_URL or http://127.0.0.1:18000."},
		"api_key":  schema.StringAttribute{Optional: true, Sensitive: true, Description: "Control-plane credential. Prefer INFERCRANE_API_KEY."},
	}}
}
func (p *inferCraneProvider) Configure(ctx context.Context, request provider.ConfigureRequest, response *provider.ConfigureResponse) {
	var config providerModel
	response.Diagnostics.Append(request.Config.Get(ctx, &config)...)
	if response.Diagnostics.HasError() || config.Endpoint.IsUnknown() || config.APIKey.IsUnknown() {
		return
	}
	endpoint, apiKey := config.Endpoint.ValueString(), config.APIKey.ValueString()
	if endpoint == "" {
		endpoint = os.Getenv("INFERCRANE_CONTROL_URL")
	}
	if apiKey == "" {
		apiKey = os.Getenv("INFERCRANE_API_KEY")
	}
	client, err := controlclient.New(endpoint, apiKey, "terraform-provider-infercrane/"+p.version)
	if err != nil {
		response.Diagnostics.AddError("Invalid InferCrane provider configuration", err.Error())
		return
	}
	response.DataSourceData, response.ResourceData = client, client
}
func (p *inferCraneProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{NewDeploymentResource}
}
func (p *inferCraneProvider) DataSources(context.Context) []func() datasource.DataSource { return nil }
func invalidClientDiagnostic() diag.Diagnostic {
	return diag.NewErrorDiagnostic("Provider is not configured", "Configure the InferCrane provider before managing a deployment.")
}
