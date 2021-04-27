package schema

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"sync"

	"github.com/hashicorp/go-cty/cty"
	ctyconvert "github.com/hashicorp/go-cty/cty/convert"
	"github.com/hashicorp/go-cty/cty/msgpack"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-sdk/v2/internal/configs/configschema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/internal/configs/hcl2shim"
	"github.com/hashicorp/terraform-plugin-sdk/v2/internal/plans/objchange"
	"github.com/hashicorp/terraform-plugin-sdk/v2/internal/plugin6/convert"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

func NewGRPCProviderServer6(p *Provider) *GRPCProviderServer6 {
	return &GRPCProviderServer6{
		provider: p,
		stopCh:   make(chan struct{}),
	}
}

// GRPCProviderServer6 handles the server, or plugin side of the rpc connection.
type GRPCProviderServer6 struct {
	provider *Provider
	stopCh   chan struct{}
	stopMu   sync.Mutex
}

// StopContext derives a new context from the passed in grpc context.
// It creates a goroutine to wait for the server stop and propagates
// cancellation to the derived grpc context.
func (s *GRPCProviderServer6) StopContext(ctx context.Context) context.Context {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()

	stoppable, cancel := context.WithCancel(ctx)
	go mergeStop(stoppable, cancel, s.stopCh)
	return stoppable
}

func (s *GRPCProviderServer6) GetProviderSchema(_ context.Context, req *tfprotov6.GetProviderSchemaRequest) (*tfprotov6.GetProviderSchemaResponse, error) {

	resp := &tfprotov6.GetProviderSchemaResponse{
		ResourceSchemas:   make(map[string]*tfprotov6.Schema),
		DataSourceSchemas: make(map[string]*tfprotov6.Schema),
	}

	resp.Provider = &tfprotov6.Schema{
		Block: convert.ConfigSchemaToProto(s.getProviderSchemaBlock()),
	}

	resp.ProviderMeta = &tfprotov6.Schema{
		Block: convert.ConfigSchemaToProto(s.getProviderMetaSchemaBlock()),
	}

	for typ, res := range s.provider.ResourcesMap {
		resp.ResourceSchemas[typ] = &tfprotov6.Schema{
			Version: int64(res.SchemaVersion),
			Block:   convert.ConfigSchemaToProto(res.CoreConfigSchema()),
		}
	}

	for typ, dat := range s.provider.DataSourcesMap {
		resp.DataSourceSchemas[typ] = &tfprotov6.Schema{
			Version: int64(dat.SchemaVersion),
			Block:   convert.ConfigSchemaToProto(dat.CoreConfigSchema()),
		}
	}

	return resp, nil
}

func (s *GRPCProviderServer6) getProviderSchemaBlock() *configschema.Block {
	return InternalMap(s.provider.Schema).CoreConfigSchema()
}

func (s *GRPCProviderServer6) getProviderMetaSchemaBlock() *configschema.Block {
	return InternalMap(s.provider.ProviderMetaSchema).CoreConfigSchema()
}

func (s *GRPCProviderServer6) getResourceSchemaBlock(name string) *configschema.Block {
	res := s.provider.ResourcesMap[name]
	return res.CoreConfigSchema()
}

func (s *GRPCProviderServer6) getDatasourceSchemaBlock(name string) *configschema.Block {
	dat := s.provider.DataSourcesMap[name]
	return dat.CoreConfigSchema()
}

func (s *GRPCProviderServer6) ValidateProviderConfig(_ context.Context, req *tfprotov6.ValidateProviderConfigRequest) (*tfprotov6.ValidateProviderConfigResponse, error) {
	resp := &tfprotov6.ValidateProviderConfigResponse{}

	schemaBlock := s.getProviderSchemaBlock()

	configVal, err := msgpack.Unmarshal(req.Config.MsgPack, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	// lookup any required, top-level attributes that are Null, and see if we
	// have a Default value available.
	configVal, err = cty.Transform(configVal, func(path cty.Path, val cty.Value) (cty.Value, error) {
		// we're only looking for top-level attributes
		if len(path) != 1 {
			return val, nil
		}

		// nothing to do if we already have a value
		if !val.IsNull() {
			return val, nil
		}

		// get the Schema definition for this attribute
		getAttr, ok := path[0].(cty.GetAttrStep)
		// these should all exist, but just ignore anything strange
		if !ok {
			return val, nil
		}

		attrSchema := s.provider.Schema[getAttr.Name]
		// continue to ignore anything that doesn't match
		if attrSchema == nil {
			return val, nil
		}

		// this is deprecated, so don't set it
		if attrSchema.Deprecated != "" {
			return val, nil
		}

		// find a default value if it exists
		def, err := attrSchema.DefaultValue()
		if err != nil {
			resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, fmt.Errorf("error getting default for %q: %s", getAttr.Name, err))
			return val, err
		}

		// no default
		if def == nil {
			return val, nil
		}

		// create a cty.Value and make sure it's the correct type
		tmpVal := hcl2shim.HCL2ValueFromConfigValue(def)

		// helper/schema used to allow setting "" to a bool
		if val.Type() == cty.Bool && tmpVal.RawEquals(cty.StringVal("")) {
			// return a warning about the conversion
			resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, "provider set empty string as default value for bool "+getAttr.Name)
			tmpVal = cty.False
		}

		val, err = ctyconvert.Convert(tmpVal, val.Type())
		if err != nil {
			resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, fmt.Errorf("error setting default for %q: %s", getAttr.Name, err))
		}

		return val, err
	})
	if err != nil {
		// any error here was already added to the diagnostics
		return resp, nil
	}

	configVal, err = schemaBlock.CoerceValue(configVal)
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	// Ensure there are no nulls that will cause helper/schema to panic.
	if err := validateConfigNulls(configVal, nil); err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	config := terraform.NewResourceConfigShimmed(configVal, schemaBlock)

	resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, s.provider.Validate(config))

	preparedConfigMP, err := msgpack.Marshal(configVal, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	resp.PreparedConfig = &tfprotov6.DynamicValue{MsgPack: preparedConfigMP}

	return resp, nil
}

func (s *GRPCProviderServer6) ValidateResourceConfig(_ context.Context, req *tfprotov6.ValidateResourceConfigRequest) (*tfprotov6.ValidateResourceConfigResponse, error) {
	resp := &tfprotov6.ValidateResourceConfigResponse{}

	schemaBlock := s.getResourceSchemaBlock(req.TypeName)

	configVal, err := msgpack.Unmarshal(req.Config.MsgPack, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	config := terraform.NewResourceConfigShimmed(configVal, schemaBlock)

	resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, s.provider.ValidateResource(req.TypeName, config))

	return resp, nil
}

func (s *GRPCProviderServer6) ValidateDataResourceConfig(_ context.Context, req *tfprotov6.ValidateDataResourceConfigRequest) (*tfprotov6.ValidateDataResourceConfigResponse, error) {
	resp := &tfprotov6.ValidateDataResourceConfigResponse{}

	schemaBlock := s.getDatasourceSchemaBlock(req.TypeName)

	configVal, err := msgpack.Unmarshal(req.Config.MsgPack, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	// Ensure there are no nulls that will cause helper/schema to panic.
	if err := validateConfigNulls(configVal, nil); err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	config := terraform.NewResourceConfigShimmed(configVal, schemaBlock)

	resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, s.provider.ValidateDataSource(req.TypeName, config))

	return resp, nil
}

func (s *GRPCProviderServer6) UpgradeResourceState(ctx context.Context, req *tfprotov6.UpgradeResourceStateRequest) (*tfprotov6.UpgradeResourceStateResponse, error) {
	resp := &tfprotov6.UpgradeResourceStateResponse{}

	res, ok := s.provider.ResourcesMap[req.TypeName]
	if !ok {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, fmt.Errorf("unknown resource type: %s", req.TypeName))
		return resp, nil
	}
	schemaBlock := s.getResourceSchemaBlock(req.TypeName)

	version := int(req.Version)

	jsonMap := map[string]interface{}{}
	var err error

	switch {
	// We first need to upgrade a flatmap state if it exists.
	// There should never be both a JSON and Flatmap state in the request.
	case len(req.RawState.Flatmap) > 0:
		jsonMap, version, err = s.upgradeFlatmapState(ctx, version, req.RawState.Flatmap, res)
		if err != nil {
			resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
			return resp, nil
		}
	// if there's a JSON state, we need to decode it.
	case len(req.RawState.JSON) > 0:
		if res.UseJSONNumber {
			err = unmarshalJSON(req.RawState.JSON, &jsonMap)
		} else {
			err = json.Unmarshal(req.RawState.JSON, &jsonMap)
		}
		if err != nil {
			resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
			return resp, nil
		}
	default:
		log.Println("[DEBUG] no state provided to upgrade")
		return resp, nil
	}

	// complete the upgrade of the JSON states
	jsonMap, err = s.upgradeJSONState(ctx, version, jsonMap, res)
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	// The provider isn't required to clean out removed fields
	s.removeAttributes(jsonMap, schemaBlock.ImpliedType())

	// now we need to turn the state into the default json representation, so
	// that it can be re-decoded using the actual schema.
	val, err := JSONMapToStateValue(jsonMap, schemaBlock)
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	// Now we need to make sure blocks are represented correctly, which means
	// that missing blocks are empty collections, rather than null.
	// First we need to CoerceValue to ensure that all object types match.
	val, err = schemaBlock.CoerceValue(val)
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}
	// Normalize the value and fill in any missing blocks.
	val = objchange.NormalizeObjectFromLegacySDK(val, schemaBlock)

	// encode the final state to the expected msgpack format
	newStateMP, err := msgpack.Marshal(val, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	resp.UpgradedState = &tfprotov6.DynamicValue{MsgPack: newStateMP}
	return resp, nil
}

// upgradeFlatmapState takes a legacy flatmap state, upgrades it using Migrate
// state if necessary, and converts it to the new JSON state format decoded as a
// map[string]interface{}.
// upgradeFlatmapState returns the json map along with the corresponding schema
// version.
func (s *GRPCProviderServer6) upgradeFlatmapState(ctx context.Context, version int, m map[string]string, res *Resource) (map[string]interface{}, int, error) {
	// this will be the version we've upgraded so, defaulting to the given
	// version in case no migration was called.
	upgradedVersion := version

	// first determine if we need to call the legacy MigrateState func
	requiresMigrate := version < res.SchemaVersion

	schemaType := res.CoreConfigSchema().ImpliedType()

	// if there are any StateUpgraders, then we need to only compare
	// against the first version there
	if len(res.StateUpgraders) > 0 {
		requiresMigrate = version < res.StateUpgraders[0].Version
	}

	if requiresMigrate && res.MigrateState == nil {
		// Providers were previously allowed to bump the version
		// without declaring MigrateState.
		// If there are further upgraders, then we've only updated that far.
		if len(res.StateUpgraders) > 0 {
			schemaType = res.StateUpgraders[0].Type
			upgradedVersion = res.StateUpgraders[0].Version
		}
	} else if requiresMigrate {
		is := &terraform.InstanceState{
			ID:         m["id"],
			Attributes: m,
			Meta: map[string]interface{}{
				"schema_version": strconv.Itoa(version),
			},
		}
		is, err := res.MigrateState(version, is, s.provider.Meta())
		if err != nil {
			return nil, 0, err
		}

		// re-assign the map in case there was a copy made, making sure to keep
		// the ID
		m := is.Attributes
		m["id"] = is.ID

		// if there are further upgraders, then we've only updated that far
		if len(res.StateUpgraders) > 0 {
			schemaType = res.StateUpgraders[0].Type
			upgradedVersion = res.StateUpgraders[0].Version
		}
	} else {
		// the schema version may be newer than the MigrateState functions
		// handled and older than the current, but still stored in the flatmap
		// form. If that's the case, we need to find the correct schema type to
		// convert the state.
		for _, upgrader := range res.StateUpgraders {
			if upgrader.Version == version {
				schemaType = upgrader.Type
				break
			}
		}
	}

	// now we know the state is up to the latest version that handled the
	// flatmap format state. Now we can upgrade the format and continue from
	// there.
	newConfigVal, err := hcl2shim.HCL2ValueFromFlatmap(m, schemaType)
	if err != nil {
		return nil, 0, err
	}

	jsonMap, err := stateValueToJSONMap(newConfigVal, schemaType, res.UseJSONNumber)
	return jsonMap, upgradedVersion, err
}

func (s *GRPCProviderServer6) upgradeJSONState(ctx context.Context, version int, m map[string]interface{}, res *Resource) (map[string]interface{}, error) {
	var err error

	for _, upgrader := range res.StateUpgraders {
		if version != upgrader.Version {
			continue
		}

		m, err = upgrader.Upgrade(ctx, m, s.provider.Meta())
		if err != nil {
			return nil, err
		}
		version++
	}

	return m, nil
}

// Remove any attributes no longer present in the schema, so that the json can
// be correctly decoded.
func (s *GRPCProviderServer6) removeAttributes(v interface{}, ty cty.Type) {
	// we're only concerned with finding maps that corespond to object
	// attributes
	switch v := v.(type) {
	case []interface{}:
		// If these aren't blocks the next call will be a noop
		if ty.IsListType() || ty.IsSetType() {
			eTy := ty.ElementType()
			for _, eV := range v {
				s.removeAttributes(eV, eTy)
			}
		}
		return
	case map[string]interface{}:
		// map blocks aren't yet supported, but handle this just in case
		if ty.IsMapType() {
			eTy := ty.ElementType()
			for _, eV := range v {
				s.removeAttributes(eV, eTy)
			}
			return
		}

		if ty == cty.DynamicPseudoType {
			log.Printf("[DEBUG] ignoring dynamic block: %#v\n", v)
			return
		}

		if !ty.IsObjectType() {
			// This shouldn't happen, and will fail to decode further on, so
			// there's no need to handle it here.
			log.Printf("[WARN] unexpected type %#v for map in json state", ty)
			return
		}

		attrTypes := ty.AttributeTypes()
		for attr, attrV := range v {
			attrTy, ok := attrTypes[attr]
			if !ok {
				log.Printf("[DEBUG] attribute %q no longer present in schema", attr)
				delete(v, attr)
				continue
			}

			s.removeAttributes(attrV, attrTy)
		}
	}
}

func (s *GRPCProviderServer6) StopProvider(_ context.Context, _ *tfprotov6.StopProviderRequest) (*tfprotov6.StopProviderResponse, error) {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()

	// stop
	close(s.stopCh)
	// reset the stop signal
	s.stopCh = make(chan struct{})

	return &tfprotov6.StopProviderResponse{}, nil
}

func (s *GRPCProviderServer6) ConfigureProvider(ctx context.Context, req *tfprotov6.ConfigureProviderRequest) (*tfprotov6.ConfigureProviderResponse, error) {
	resp := &tfprotov6.ConfigureProviderResponse{}

	schemaBlock := s.getProviderSchemaBlock()

	configVal, err := msgpack.Unmarshal(req.Config.MsgPack, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	s.provider.TerraformVersion = req.TerraformVersion

	// Ensure there are no nulls that will cause helper/schema to panic.
	if err := validateConfigNulls(configVal, nil); err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	config := terraform.NewResourceConfigShimmed(configVal, schemaBlock)
	// TODO: remove global stop context hack
	// This attaches a global stop synchro'd context onto the provider.Configure
	// request scoped context. This provides a substitute for the removed provider.StopContext()
	// function. Ideally a provider should migrate to the context aware API that receives
	// request scoped contexts, however this is a large undertaking for very large providers.
	ctxHack := context.WithValue(ctx, StopContextKey, s.StopContext(context.Background()))
	diags := s.provider.Configure(ctxHack, config)
	resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, diags)

	return resp, nil
}

func (s *GRPCProviderServer6) ReadResource(ctx context.Context, req *tfprotov6.ReadResourceRequest) (*tfprotov6.ReadResourceResponse, error) {
	resp := &tfprotov6.ReadResourceResponse{
		// helper/schema did previously handle private data during refresh, but
		// core is now going to expect this to be maintained in order to
		// persist it in the state.
		Private: req.Private,
	}

	res, ok := s.provider.ResourcesMap[req.TypeName]
	if !ok {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, fmt.Errorf("unknown resource type: %s", req.TypeName))
		return resp, nil
	}
	schemaBlock := s.getResourceSchemaBlock(req.TypeName)

	stateVal, err := msgpack.Unmarshal(req.CurrentState.MsgPack, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	instanceState, err := res.ShimInstanceStateFromValue(stateVal)
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	private := make(map[string]interface{})
	if len(req.Private) > 0 {
		if err := json.Unmarshal(req.Private, &private); err != nil {
			resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
			return resp, nil
		}
	}
	instanceState.Meta = private

	pmSchemaBlock := s.getProviderMetaSchemaBlock()
	if pmSchemaBlock != nil && req.ProviderMeta != nil {
		providerSchemaVal, err := msgpack.Unmarshal(req.ProviderMeta.MsgPack, pmSchemaBlock.ImpliedType())
		if err != nil {
			resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
			return resp, nil
		}
		instanceState.ProviderMeta = providerSchemaVal
	}

	newInstanceState, diags := res.RefreshWithoutUpgrade(ctx, instanceState, s.provider.Meta())
	resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, diags)
	if diags.HasError() {
		return resp, nil
	}

	if newInstanceState == nil || newInstanceState.ID == "" {
		// The old provider API used an empty id to signal that the remote
		// object appears to have been deleted, but our new protocol expects
		// to see a null value (in the cty sense) in that case.
		newStateMP, err := msgpack.Marshal(cty.NullVal(schemaBlock.ImpliedType()), schemaBlock.ImpliedType())
		if err != nil {
			resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		}
		resp.NewState = &tfprotov6.DynamicValue{
			MsgPack: newStateMP,
		}
		return resp, nil
	}

	// helper/schema should always copy the ID over, but do it again just to be safe
	newInstanceState.Attributes["id"] = newInstanceState.ID

	newStateVal, err := hcl2shim.HCL2ValueFromFlatmap(newInstanceState.Attributes, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	newStateVal = normalizeNullValues(newStateVal, stateVal, false)
	newStateVal = copyTimeoutValues(newStateVal, stateVal)

	newStateMP, err := msgpack.Marshal(newStateVal, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	resp.NewState = &tfprotov6.DynamicValue{
		MsgPack: newStateMP,
	}

	return resp, nil
}

func (s *GRPCProviderServer6) PlanResourceChange(ctx context.Context, req *tfprotov6.PlanResourceChangeRequest) (*tfprotov6.PlanResourceChangeResponse, error) {
	resp := &tfprotov6.PlanResourceChangeResponse{}

	// This is a signal to Terraform Core that we're doing the best we can to
	// shim the legacy type system of the SDK onto the Terraform type system
	// but we need it to cut us some slack. This setting should not be taken
	// forward to any new SDK implementations, since setting it prevents us
	// from catching certain classes of provider bug that can lead to
	// confusing downstream errors.
	resp.UnsafeToUseLegacyTypeSystem = true

	res, ok := s.provider.ResourcesMap[req.TypeName]
	if !ok {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, fmt.Errorf("unknown resource type: %s", req.TypeName))
		return resp, nil
	}
	schemaBlock := s.getResourceSchemaBlock(req.TypeName)

	priorStateVal, err := msgpack.Unmarshal(req.PriorState.MsgPack, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	create := priorStateVal.IsNull()

	proposedNewStateVal, err := msgpack.Unmarshal(req.ProposedNewState.MsgPack, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	// We don't usually plan destroys, but this can return early in any case.
	if proposedNewStateVal.IsNull() {
		resp.PlannedState = req.ProposedNewState
		resp.PlannedPrivate = req.PriorPrivate
		return resp, nil
	}

	priorState, err := res.ShimInstanceStateFromValue(priorStateVal)
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}
	priorPrivate := make(map[string]interface{})
	if len(req.PriorPrivate) > 0 {
		if err := json.Unmarshal(req.PriorPrivate, &priorPrivate); err != nil {
			resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
			return resp, nil
		}
	}

	priorState.Meta = priorPrivate

	pmSchemaBlock := s.getProviderMetaSchemaBlock()
	if pmSchemaBlock != nil && req.ProviderMeta != nil {
		providerSchemaVal, err := msgpack.Unmarshal(req.ProviderMeta.MsgPack, pmSchemaBlock.ImpliedType())
		if err != nil {
			resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
			return resp, nil
		}
		priorState.ProviderMeta = providerSchemaVal
	}

	// Ensure there are no nulls that will cause helper/schema to panic.
	if err := validateConfigNulls(proposedNewStateVal, nil); err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	// turn the proposed state into a legacy configuration
	cfg := terraform.NewResourceConfigShimmed(proposedNewStateVal, schemaBlock)

	diff, err := res.SimpleDiff(ctx, priorState, cfg, s.provider.Meta())
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	// if this is a new instance, we need to make sure ID is going to be computed
	if create {
		if diff == nil {
			diff = terraform.NewInstanceDiff()
		}

		diff.Attributes["id"] = &terraform.ResourceAttrDiff{
			NewComputed: true,
		}
	}

	if diff == nil || len(diff.Attributes) == 0 {
		// schema.Provider.Diff returns nil if it ends up making a diff with no
		// changes, but our new interface wants us to return an actual change
		// description that _shows_ there are no changes. This is always the
		// prior state, because we force a diff above if this is a new instance.
		resp.PlannedState = req.PriorState
		resp.PlannedPrivate = req.PriorPrivate
		return resp, nil
	}

	if priorState == nil {
		priorState = &terraform.InstanceState{}
	}

	// now we need to apply the diff to the prior state, so get the planned state
	plannedAttrs, err := diff.Apply(priorState.Attributes, schemaBlock)

	plannedStateVal, err := hcl2shim.HCL2ValueFromFlatmap(plannedAttrs, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	plannedStateVal, err = schemaBlock.CoerceValue(plannedStateVal)
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	plannedStateVal = normalizeNullValues(plannedStateVal, proposedNewStateVal, false)

	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	plannedStateVal = copyTimeoutValues(plannedStateVal, proposedNewStateVal)

	// The old SDK code has some imprecisions that cause it to sometimes
	// generate differences that the SDK itself does not consider significant
	// but Terraform Core would. To avoid producing weird do-nothing diffs
	// in that case, we'll check if the provider as produced something we
	// think is "equivalent" to the prior state and just return the prior state
	// itself if so, thus ensuring that Terraform Core will treat this as
	// a no-op. See the docs for ValuesSDKEquivalent for some caveats on its
	// accuracy.
	forceNoChanges := false
	if hcl2shim.ValuesSDKEquivalent(priorStateVal, plannedStateVal) {
		plannedStateVal = priorStateVal
		forceNoChanges = true
	}

	// if this was creating the resource, we need to set any remaining computed
	// fields
	if create {
		plannedStateVal = SetUnknowns(plannedStateVal, schemaBlock)
	}

	plannedMP, err := msgpack.Marshal(plannedStateVal, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}
	resp.PlannedState = &tfprotov6.DynamicValue{
		MsgPack: plannedMP,
	}

	// encode any timeouts into the diff Meta
	t := &ResourceTimeout{}
	if err := t.ConfigDecode(res, cfg); err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	if err := t.DiffEncode(diff); err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	// Now we need to store any NewExtra values, which are where any actual
	// StateFunc modified config fields are hidden.
	privateMap := diff.Meta
	if privateMap == nil {
		privateMap = map[string]interface{}{}
	}

	newExtra := map[string]interface{}{}

	for k, v := range diff.Attributes {
		if v.NewExtra != nil {
			newExtra[k] = v.NewExtra
		}
	}
	privateMap[newExtraKey] = newExtra

	// the Meta field gets encoded into PlannedPrivate
	plannedPrivate, err := json.Marshal(privateMap)
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}
	resp.PlannedPrivate = plannedPrivate

	// collect the attributes that require instance replacement, and convert
	// them to cty.Paths.
	var requiresNew []string
	if !forceNoChanges {
		for attr, d := range diff.Attributes {
			if d.RequiresNew {
				requiresNew = append(requiresNew, attr)
			}
		}
	}

	// If anything requires a new resource already, or the "id" field indicates
	// that we will be creating a new resource, then we need to add that to
	// RequiresReplace so that core can tell if the instance is being replaced
	// even if changes are being suppressed via "ignore_changes".
	id := plannedStateVal.GetAttr("id")
	if len(requiresNew) > 0 || id.IsNull() || !id.IsKnown() {
		requiresNew = append(requiresNew, "id")
	}

	requiresReplace, err := hcl2shim.RequiresReplace(requiresNew, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	// convert these to the protocol structures
	for _, p := range requiresReplace {
		resp.RequiresReplace = append(resp.RequiresReplace, pathToAttributePath(p))
	}

	return resp, nil
}

func (s *GRPCProviderServer6) ApplyResourceChange(ctx context.Context, req *tfprotov6.ApplyResourceChangeRequest) (*tfprotov6.ApplyResourceChangeResponse, error) {
	resp := &tfprotov6.ApplyResourceChangeResponse{
		// Start with the existing state as a fallback
		NewState: req.PriorState,
	}

	res, ok := s.provider.ResourcesMap[req.TypeName]
	if !ok {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, fmt.Errorf("unknown resource type: %s", req.TypeName))
		return resp, nil
	}
	schemaBlock := s.getResourceSchemaBlock(req.TypeName)

	priorStateVal, err := msgpack.Unmarshal(req.PriorState.MsgPack, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	plannedStateVal, err := msgpack.Unmarshal(req.PlannedState.MsgPack, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	priorState, err := res.ShimInstanceStateFromValue(priorStateVal)
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	private := make(map[string]interface{})
	if len(req.PlannedPrivate) > 0 {
		if err := json.Unmarshal(req.PlannedPrivate, &private); err != nil {
			resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
			return resp, nil
		}
	}

	var diff *terraform.InstanceDiff
	destroy := false

	// a null state means we are destroying the instance
	if plannedStateVal.IsNull() {
		destroy = true
		diff = &terraform.InstanceDiff{
			Attributes: make(map[string]*terraform.ResourceAttrDiff),
			Meta:       make(map[string]interface{}),
			Destroy:    true,
		}
	} else {
		diff, err = DiffFromValues(ctx, priorStateVal, plannedStateVal, stripResourceModifiers(res))
		if err != nil {
			resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
			return resp, nil
		}
	}

	if diff == nil {
		diff = &terraform.InstanceDiff{
			Attributes: make(map[string]*terraform.ResourceAttrDiff),
			Meta:       make(map[string]interface{}),
		}
	}

	// add NewExtra Fields that may have been stored in the private data
	if newExtra := private[newExtraKey]; newExtra != nil {
		for k, v := range newExtra.(map[string]interface{}) {
			d := diff.Attributes[k]

			if d == nil {
				d = &terraform.ResourceAttrDiff{}
			}

			d.NewExtra = v
			diff.Attributes[k] = d
		}
	}

	if private != nil {
		diff.Meta = private
	}

	for k, d := range diff.Attributes {
		// We need to turn off any RequiresNew. There could be attributes
		// without changes in here inserted by helper/schema, but if they have
		// RequiresNew then the state will be dropped from the ResourceData.
		d.RequiresNew = false

		// Check that any "removed" attributes that don't actually exist in the
		// prior state, or helper/schema will confuse itself
		if d.NewRemoved {
			if _, ok := priorState.Attributes[k]; !ok {
				delete(diff.Attributes, k)
			}
		}
	}

	pmSchemaBlock := s.getProviderMetaSchemaBlock()
	if pmSchemaBlock != nil && req.ProviderMeta != nil {
		providerSchemaVal, err := msgpack.Unmarshal(req.ProviderMeta.MsgPack, pmSchemaBlock.ImpliedType())
		if err != nil {
			resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
			return resp, nil
		}
		priorState.ProviderMeta = providerSchemaVal
	}

	newInstanceState, diags := res.Apply(ctx, priorState, diff, s.provider.Meta())
	resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, diags)

	newStateVal := cty.NullVal(schemaBlock.ImpliedType())

	// Always return a null value for destroy.
	// While this is usually indicated by a nil state, check for missing ID or
	// attributes in the case of a provider failure.
	if destroy || newInstanceState == nil || newInstanceState.Attributes == nil || newInstanceState.ID == "" {
		newStateMP, err := msgpack.Marshal(newStateVal, schemaBlock.ImpliedType())
		if err != nil {
			resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
			return resp, nil
		}
		resp.NewState = &tfprotov6.DynamicValue{
			MsgPack: newStateMP,
		}
		return resp, nil
	}

	// We keep the null val if we destroyed the resource, otherwise build the
	// entire object, even if the new state was nil.
	newStateVal, err = StateValueFromInstanceState(newInstanceState, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	newStateVal = normalizeNullValues(newStateVal, plannedStateVal, true)

	newStateVal = copyTimeoutValues(newStateVal, plannedStateVal)

	newStateMP, err := msgpack.Marshal(newStateVal, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}
	resp.NewState = &tfprotov6.DynamicValue{
		MsgPack: newStateMP,
	}

	meta, err := json.Marshal(newInstanceState.Meta)
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}
	resp.Private = meta

	// This is a signal to Terraform Core that we're doing the best we can to
	// shim the legacy type system of the SDK onto the Terraform type system
	// but we need it to cut us some slack. This setting should not be taken
	// forward to any new SDK implementations, since setting it prevents us
	// from catching certain classes of provider bug that can lead to
	// confusing downstream errors.
	resp.UnsafeToUseLegacyTypeSystem = true

	return resp, nil
}

func (s *GRPCProviderServer6) ImportResourceState(ctx context.Context, req *tfprotov6.ImportResourceStateRequest) (*tfprotov6.ImportResourceStateResponse, error) {
	resp := &tfprotov6.ImportResourceStateResponse{}

	info := &terraform.InstanceInfo{
		Type: req.TypeName,
	}

	newInstanceStates, err := s.provider.ImportState(ctx, info, req.ID)
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	for _, is := range newInstanceStates {
		// copy the ID again just to be sure it wasn't missed
		is.Attributes["id"] = is.ID

		resourceType := is.Ephemeral.Type
		if resourceType == "" {
			resourceType = req.TypeName
		}

		schemaBlock := s.getResourceSchemaBlock(resourceType)
		newStateVal, err := hcl2shim.HCL2ValueFromFlatmap(is.Attributes, schemaBlock.ImpliedType())
		if err != nil {
			resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
			return resp, nil
		}

		// Normalize the value and fill in any missing blocks.
		newStateVal = objchange.NormalizeObjectFromLegacySDK(newStateVal, schemaBlock)

		newStateMP, err := msgpack.Marshal(newStateVal, schemaBlock.ImpliedType())
		if err != nil {
			resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
			return resp, nil
		}

		meta, err := json.Marshal(is.Meta)
		if err != nil {
			resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
			return resp, nil
		}

		importedResource := &tfprotov6.ImportedResource{
			TypeName: resourceType,
			State: &tfprotov6.DynamicValue{
				MsgPack: newStateMP,
			},
			Private: meta,
		}

		resp.ImportedResources = append(resp.ImportedResources, importedResource)
	}

	return resp, nil
}

func (s *GRPCProviderServer6) ReadDataSource(ctx context.Context, req *tfprotov6.ReadDataSourceRequest) (*tfprotov6.ReadDataSourceResponse, error) {
	resp := &tfprotov6.ReadDataSourceResponse{}

	schemaBlock := s.getDatasourceSchemaBlock(req.TypeName)

	configVal, err := msgpack.Unmarshal(req.Config.MsgPack, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	// Ensure there are no nulls that will cause helper/schema to panic.
	if err := validateConfigNulls(configVal, nil); err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	config := terraform.NewResourceConfigShimmed(configVal, schemaBlock)

	// we need to still build the diff separately with the Read method to match
	// the old behavior
	res, ok := s.provider.DataSourcesMap[req.TypeName]
	if !ok {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, fmt.Errorf("unknown data source: %s", req.TypeName))
		return resp, nil
	}
	diff, err := res.Diff(ctx, nil, config, s.provider.Meta())
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	// now we can get the new complete data source
	newInstanceState, diags := res.ReadDataApply(ctx, diff, s.provider.Meta())
	resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, diags)
	if diags.HasError() {
		return resp, nil
	}

	newStateVal, err := StateValueFromInstanceState(newInstanceState, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}

	newStateVal = copyTimeoutValues(newStateVal, configVal)

	newStateMP, err := msgpack.Marshal(newStateVal, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = convert.AppendProtoDiag(resp.Diagnostics, err)
		return resp, nil
	}
	resp.State = &tfprotov6.DynamicValue{
		MsgPack: newStateMP,
	}
	return resp, nil
}
