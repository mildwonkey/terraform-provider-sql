package convert

import (
	"fmt"

	"github.com/hashicorp/go-cty/cty"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
)

// AppendProtoDiag appends a new diagnostic from a warning string or an error.
// This panics if d is not a string or error.
func AppendProtoDiag(diags []*tfprotov6.Diagnostic, d interface{}) []*tfprotov6.Diagnostic {
	switch d := d.(type) {
	case cty.PathError:
		ap := PathToAttributePath(d.Path)
		diags = append(diags, &tfprotov6.Diagnostic{
			Severity:  tfprotov6.DiagnosticSeverityError,
			Summary:   d.Error(),
			Attribute: ap,
		})
	case diag.Diagnostics:
		diags = append(diags, DiagsToProto(d)...)
	case error:
		diags = append(diags, &tfprotov6.Diagnostic{
			Severity: tfprotov6.DiagnosticSeverityError,
			Summary:  d.Error(),
		})
	case string:
		diags = append(diags, &tfprotov6.Diagnostic{
			Severity: tfprotov6.DiagnosticSeverityWarning,
			Summary:  d,
		})
	case *tfprotov6.Diagnostic:
		diags = append(diags, d)
	case []*tfprotov6.Diagnostic:
		diags = append(diags, d...)
	}
	return diags
}

// ProtoToDiags converts a list of tfprotov6.Diagnostics to a diag.Diagnostics.
func ProtoToDiags(ds []*tfprotov6.Diagnostic) diag.Diagnostics {
	var diags diag.Diagnostics
	for _, d := range ds {
		var severity diag.Severity

		switch d.Severity {
		case tfprotov6.DiagnosticSeverityError:
			severity = diag.Error
		case tfprotov6.DiagnosticSeverityWarning:
			severity = diag.Warning
		}

		diags = append(diags, diag.Diagnostic{
			Severity:      severity,
			Summary:       d.Summary,
			Detail:        d.Detail,
			AttributePath: AttributePathToPath(d.Attribute),
		})
	}

	return diags
}

func DiagsToProto(diags diag.Diagnostics) []*tfprotov6.Diagnostic {
	var ds []*tfprotov6.Diagnostic
	for _, d := range diags {
		if err := d.Validate(); err != nil {
			panic(fmt.Errorf("Invalid diagnostic: %s. This is always a bug in the provider implementation", err))
		}
		protoDiag := &tfprotov6.Diagnostic{
			Summary:   d.Summary,
			Detail:    d.Detail,
			Attribute: PathToAttributePath(d.AttributePath),
		}
		if d.Severity == diag.Error {
			protoDiag.Severity = tfprotov6.DiagnosticSeverityError
		} else if d.Severity == diag.Warning {
			protoDiag.Severity = tfprotov6.DiagnosticSeverityWarning
		}
		ds = append(ds, protoDiag)
	}
	return ds
}

// AttributePathToPath takes the proto encoded path and converts it to a cty.Path
func AttributePathToPath(ap *tftypes.AttributePath) cty.Path {
	var p cty.Path
	if ap == nil {
		return p
	}
	for _, step := range ap.Steps() {
		switch step.(type) {
		case tftypes.AttributeName:
			p = p.GetAttr(string(step.(tftypes.AttributeName)))
		case tftypes.ElementKeyString:
			p = p.Index(cty.StringVal(string(step.(tftypes.ElementKeyString))))
		case tftypes.ElementKeyInt:
			p = p.Index(cty.NumberIntVal(int64(step.(tftypes.ElementKeyInt))))
		}
	}
	return p
}

// PathToAttributePath takes a cty.Path and converts it to a proto-encoded path.
func PathToAttributePath(p cty.Path) *tftypes.AttributePath {
	if p == nil || len(p) < 1 {
		return nil
	}
	var steps []tftypes.AttributePathStep

	for _, step := range p {
		switch selector := step.(type) {
		case cty.GetAttrStep:
			steps = append(steps, tftypes.AttributeName(selector.Name))

		case cty.IndexStep:
			key := selector.Key
			switch key.Type() {
			case cty.String:
				steps = append(steps, tftypes.ElementKeyString(key.AsString()))
			case cty.Number:
				v, _ := key.AsBigFloat().Int64()
				steps = append(steps, tftypes.ElementKeyInt(v))
			default:
				// We'll bail early if we encounter anything else, and just
				// return the valid prefix.
				return &tftypes.AttributePath{}
			}
		}
	}
	return tftypes.NewAttributePathWithSteps(steps)
}
