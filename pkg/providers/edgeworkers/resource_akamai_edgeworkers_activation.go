package edgeworkers

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/akamai/AkamaiOPEN-edgegrid-golang/v2/pkg/edgeworkers"
	"github.com/akamai/AkamaiOPEN-edgegrid-golang/v2/pkg/session"
	"github.com/akamai/terraform-provider-akamai/v2/pkg/akamai"
	"github.com/akamai/terraform-provider-akamai/v2/pkg/tools"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
)

func resourceEdgeworkersActivation() *schema.Resource {
	return &schema.Resource{
		CreateContext: resourceEdgeworkersActivationCreate,
		ReadContext:   resourceEdgeworkersActivationRead,
		UpdateContext: resourceEdgeworkersActivationUpdate,
		DeleteContext: resourceEdgeworkersActivationDelete,
		Schema:        resourceEdgeworkersActivationSchema(),
		Timeouts: &schema.ResourceTimeout{
			Default: &edgeworkersActivationResourceTimeout,
		},
	}
}

func resourceEdgeworkersActivationSchema() map[string]*schema.Schema {
	return map[string]*schema.Schema{
		"edgeworker_id": {
			Type:        schema.TypeInt,
			Required:    true,
			ForceNew:    true,
			Description: "Id of the EdgeWorker to activate",
		},
		"version": {
			Type:        schema.TypeString,
			Required:    true,
			Description: "The version of EdgeWorker to activate",
		},
		"network": {
			Type:     schema.TypeString,
			Required: true,
			ValidateDiagFunc: validation.ToDiagFunc(validation.StringInSlice([]string{
				string(edgeworkers.ActivationNetworkStaging),
				string(edgeworkers.ActivationNetworkProduction),
			}, false)),
			Description: "The network on which the version will be activated",
		},
		"activation_id": {
			Type:        schema.TypeInt,
			Computed:    true,
			Description: "A unique identifier of the activation",
		},
	}
}

var (
	activationStatusComplete             = "COMPLETE"
	activationStatusPresubmit            = "PRESUBMIT"
	activationStatusPending              = "PENDING"
	activationStatusInProgress           = "IN_PROGRESS"
	errorCodeVersionIsBeingDeactivated   = "EW1031"
	errorCodeVersionAlreadyDeactivated   = "EW1032"
	activationPollMinimum                = time.Minute
	activationPollInterval               = activationPollMinimum
	edgeworkersActivationResourceTimeout = time.Minute * 30
)

func resourceEdgeworkersActivationCreate(ctx context.Context, rd *schema.ResourceData, m interface{}) diag.Diagnostics {
	meta := akamai.Meta(m)
	logger := meta.Log("Edgeworkers", "resourceEdgeworkersActivationCreate")
	ctx = session.ContextWithOptions(ctx, session.WithContextLog(logger))
	client := inst.Client(meta)

	logger.Debug("Activating edgeworker")

	edgeworkerID, err := tools.GetIntValue("edgeworker_id", rd)
	if err != nil {
		return diag.FromErr(err)
	}

	version, err := tools.GetStringValue("version", rd)
	if err != nil {
		return diag.FromErr(err)
	}

	network, err := tools.GetStringValue("network", rd)
	if err != nil {
		return diag.FromErr(err)
	}

	versionsResp, err := client.ListEdgeWorkerVersions(ctx, edgeworkers.ListEdgeWorkerVersionsRequest{
		EdgeWorkerID: edgeworkerID,
	})
	if err != nil {
		return diag.Errorf("%s: %s", ErrEdgeworkerActivation, err.Error())
	}
	if !versionExists(version, versionsResp.EdgeWorkerVersions) {
		return diag.Errorf(`%s: version '%s' is not valid for edgeworker with id=%d`, ErrEdgeworkerActivation, version, edgeworkerID)
	}

	currentActivation, err := getCurrentActivation(ctx, client, edgeworkerID, network)
	if err != nil {
		return diag.Errorf("%s: %s", ErrEdgeworkerActivation, err.Error())
	}

	if currentActivation != nil && currentActivation.Version == version {
		rd.SetId(fmt.Sprintf("%d:%s", edgeworkerID, network))
		return resourceEdgeworkersActivationRead(ctx, rd, m)
	}

	activation, err := client.ActivateVersion(ctx, edgeworkers.ActivateVersionRequest{
		EdgeWorkerID: edgeworkerID,
		ActivateVersion: edgeworkers.ActivateVersion{
			Network: edgeworkers.ActivationNetwork(network),
			Version: version,
		},
	})

	if err != nil {
		return diag.Errorf("%s: %s", ErrEdgeworkerActivation, err.Error())
	}

	if _, err := waitForEdgeworkerActivation(ctx, client, edgeworkerID, activation.ActivationID); err != nil {
		return diag.Errorf("%s: %s", ErrEdgeworkerActivation, err.Error())
	}

	rd.SetId(fmt.Sprintf("%d:%s", edgeworkerID, network))
	return resourceEdgeworkersActivationRead(ctx, rd, m)
}

func resourceEdgeworkersActivationRead(ctx context.Context, rd *schema.ResourceData, m interface{}) diag.Diagnostics {
	meta := akamai.Meta(m)
	logger := meta.Log("Edgeworkers", "resourceEdgeworkersActivationRead")
	ctx = session.ContextWithOptions(ctx, session.WithContextLog(logger))
	client := inst.Client(meta)

	logger.Debug("Reading edgeworker activations")

	edgeworkerID, err := tools.GetIntValue("edgeworker_id", rd)
	if err != nil {
		return diag.FromErr(err)
	}

	network, err := tools.GetStringValue("network", rd)
	if err != nil {
		return diag.FromErr(err)
	}

	activation, err := getCurrentActivation(ctx, client, edgeworkerID, network)
	if err != nil {
		return diag.Errorf("%s read: %s", ErrEdgeworkerActivation, err)
	}

	if activation == nil {
		return diag.Errorf(`%s read: no version active on network '%s' for edgeworker with id=%d`, ErrEdgeworkerActivation, network, edgeworkerID)
	}

	if err := rd.Set("version", activation.Version); err != nil {
		return diag.Errorf("%v: %s", tools.ErrValueSet, err.Error())
	}

	if err := rd.Set("activation_id", activation.ActivationID); err != nil {
		return diag.Errorf("%v: %s", tools.ErrValueSet, err.Error())
	}
	return nil
}

func resourceEdgeworkersActivationUpdate(ctx context.Context, rd *schema.ResourceData, m interface{}) diag.Diagnostics {
	meta := akamai.Meta(m)
	logger := meta.Log("Edgeworkers", "resourceEdgeworkersActivationUpdate")
	ctx = session.ContextWithOptions(ctx, session.WithContextLog(logger))
	client := inst.Client(meta)

	logger.Debug("Updating edgeworker activation")

	edgeworkerID, err := tools.GetIntValue("edgeworker_id", rd)
	if err != nil {
		return diag.FromErr(err)
	}

	version, err := tools.GetStringValue("version", rd)
	if err != nil {
		return diag.FromErr(err)
	}

	network, err := tools.GetStringValue("network", rd)
	if err != nil {
		return diag.FromErr(err)
	}

	versionsResp, err := client.ListEdgeWorkerVersions(ctx, edgeworkers.ListEdgeWorkerVersionsRequest{
		EdgeWorkerID: edgeworkerID,
	})
	if err != nil {
		return diag.Errorf("%s update: %s", ErrEdgeworkerActivation, err.Error())
	}
	if !versionExists(version, versionsResp.EdgeWorkerVersions) {
		return diag.Errorf(`%s update: version '%s' is not valid for edgeworker with id=%d`, ErrEdgeworkerActivation, version, edgeworkerID)
	}

	activation, err := client.ActivateVersion(ctx, edgeworkers.ActivateVersionRequest{
		EdgeWorkerID: edgeworkerID,
		ActivateVersion: edgeworkers.ActivateVersion{
			Network: edgeworkers.ActivationNetwork(network),
			Version: version,
		},
	})
	if err != nil {
		return diag.Errorf("%s: %s", ErrEdgeworkerActivation, err.Error())
	}

	if _, err := waitForEdgeworkerActivation(ctx, client, edgeworkerID, activation.ActivationID); err != nil {
		return diag.Errorf("%s: %s", ErrEdgeworkerActivation, err.Error())
	}

	rd.SetId(fmt.Sprintf("%d:%s", edgeworkerID, network))

	return resourceEdgeworkersActivationRead(ctx, rd, m)
}

func resourceEdgeworkersActivationDelete(ctx context.Context, rd *schema.ResourceData, m interface{}) diag.Diagnostics {
	meta := akamai.Meta(m)
	logger := meta.Log("Edgeworkers", "resourceEdgeworkersActivationDelete")
	ctx = session.ContextWithOptions(ctx, session.WithContextLog(logger))
	client := inst.Client(meta)

	logger.Debug("Deactivating edgeworker")

	edgeworkerID, err := tools.GetIntValue("edgeworker_id", rd)
	if err != nil {
		return diag.FromErr(err)
	}

	version, err := tools.GetStringValue("version", rd)
	if err != nil {
		return diag.FromErr(err)
	}

	network, err := tools.GetStringValue("network", rd)
	if err != nil {
		return diag.FromErr(err)
	}

	deactivation, err := client.DeactivateVersion(ctx, edgeworkers.DeactivateVersionRequest{
		EdgeWorkerID: edgeworkerID,
		DeactivateVersion: edgeworkers.DeactivateVersion{
			Version: version,
			Network: edgeworkers.ActivationNetwork(network),
		},
	})
	if err != nil {
		var e *edgeworkers.Error
		ok := errors.As(err, &e)
		if ok && e.ErrorCode == errorCodeVersionAlreadyDeactivated {
			return nil
		}
		if !ok || e.ErrorCode != errorCodeVersionIsBeingDeactivated {
			return diag.Errorf("%s: %s", ErrEdgeworkerDeactivation, err)
		}
		deactivations, err := getDeactivationsByVersionAndNetwork(ctx, client, edgeworkerID, version, network)
		if err != nil {
			return diag.Errorf("%s: %s", ErrEdgeworkerDeactivation, err)
		}
		deactivation = &deactivations[0]
	}

	if _, err := waitForEdgeworkerDeactivation(ctx, client, edgeworkerID, deactivation.DeactivationID); err != nil {
		return diag.Errorf("%s: %s", ErrEdgeworkerDeactivation, err)
	}

	rd.SetId("")
	return nil
}

func getCurrentActivation(ctx context.Context, client edgeworkers.Edgeworkers, edgeworkerID int, network string) (*edgeworkers.Activation, error) {
	activationsResp, err := client.ListActivations(ctx, edgeworkers.ListActivationsRequest{
		EdgeWorkerID: edgeworkerID,
	})
	if err != nil {
		return nil, err
	}

	activations := sortActivationsByDate(filterActivationsByNetwork(activationsResp.Activations, network))
	if len(activations) == 0 {
		return nil, nil
	}
	latestActivation := &activations[0]

	if latestActivation.Status != activationStatusComplete {
		if latestActivation.Status != activationStatusPending && latestActivation.Status != activationStatusPresubmit && latestActivation.Status != activationStatusInProgress {
			// don't return error as it is a valid state for activation
			return nil, nil
		}
		latestActivation, err = waitForEdgeworkerActivation(ctx, client, edgeworkerID, latestActivation.ActivationID)
		if err != nil {
			return nil, err
		}
		return latestActivation, nil
	}

	latestDeactivation, err := getLatestCompletedDeactivation(ctx, client, edgeworkerID, latestActivation.Version, network)
	if err != nil {
		return nil, err
	}
	if latestDeactivation == nil {
		return latestActivation, nil
	}

	timeLayout := time.RFC3339
	activationTime, err := time.Parse(timeLayout, latestActivation.CreatedTime)
	if err != nil {
		return nil, fmt.Errorf("failed to parse activation time")
	}
	deactivationTime, err := time.Parse(timeLayout, latestDeactivation.CreatedTime)
	if err != nil {
		return nil, fmt.Errorf("failed to parse deactivation time")
	}

	if deactivationTime.After(activationTime) {
		return nil, nil
	}

	return latestActivation, nil
}

func getDeactivationsByVersionAndNetwork(ctx context.Context, client edgeworkers.Edgeworkers, edgeworkerID int, version, network string) ([]edgeworkers.Deactivation, error) {
	deactivationsResp, err := client.ListDeactivations(ctx, edgeworkers.ListDeactivationsRequest{
		EdgeWorkerID: edgeworkerID,
		Version:      version,
	})
	if err != nil {
		return nil, err
	}

	return sortDeactivationsByDate(filterDeactivationsByNetwork(deactivationsResp.Deactivations, network)), nil
}

func getLatestCompletedDeactivation(ctx context.Context, client edgeworkers.Edgeworkers, edgeworkerID int, version, network string) (*edgeworkers.Deactivation, error) {
	deactivations, err := getDeactivationsByVersionAndNetwork(ctx, client, edgeworkerID, version, network)
	if err != nil {
		return nil, err
	}
	if len(deactivations) == 0 {
		return nil, nil
	}

	for i := range deactivations {
		d := &deactivations[i]
		if d.Status == activationStatusPending || d.Status == activationStatusPresubmit || d.Status == activationStatusInProgress {
			d, err = waitForEdgeworkerDeactivation(ctx, client, edgeworkerID, d.DeactivationID)
			if err != nil {
				return nil, err
			}
		}
		if d.Status == activationStatusComplete {
			return d, nil
		}
	}
	return nil, nil
}

func versionExists(version string, versions []edgeworkers.EdgeWorkerVersion) bool {
	for _, v := range versions {
		if v.Version == version {
			return true
		}
	}
	return false
}

func waitForEdgeworkerActivation(ctx context.Context, client edgeworkers.Edgeworkers, edgeworkerID, activationID int) (*edgeworkers.Activation, error) {
	activation, err := client.GetActivation(ctx, edgeworkers.GetActivationRequest{
		EdgeWorkerID: edgeworkerID,
		ActivationID: activationID,
	})
	if err != nil {
		return nil, err
	}
	for activation != nil && activation.Status != activationStatusComplete {
		if activation.Status != activationStatusPresubmit && activation.Status != activationStatusPending && activation.Status != activationStatusInProgress {
			return nil, ErrEdgeworkerActivationFailure
		}
		select {
		case <-time.After(tools.MaxDuration(activationPollInterval, activationPollMinimum)):
			activation, err = client.GetActivation(ctx, edgeworkers.GetActivationRequest{
				EdgeWorkerID: edgeworkerID,
				ActivationID: activationID,
			})
			if err != nil {
				return nil, err
			}
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, ErrEdgeworkerActivationTimeout
			}
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil, ErrEdgeworkerActivationCancelled
			}
			return nil, fmt.Errorf("%v: %w", ErrEdgeworkerActivationContextTerminated, ctx.Err())
		}
	}
	return activation, nil
}

func waitForEdgeworkerDeactivation(ctx context.Context, client edgeworkers.Edgeworkers, edgeworkerID, deactivationID int) (*edgeworkers.Deactivation, error) {
	deactivation, err := client.GetDeactivation(ctx, edgeworkers.GetDeactivationRequest{
		EdgeWorkerID:   edgeworkerID,
		DeactivationID: deactivationID,
	})
	if err != nil {
		return nil, err
	}
	for deactivation != nil && deactivation.Status != activationStatusComplete {
		if deactivation.Status != activationStatusPresubmit && deactivation.Status != activationStatusPending && deactivation.Status != activationStatusInProgress {
			return nil, ErrEdgeworkerDeactivationFailure
		}
		select {
		case <-time.After(tools.MaxDuration(activationPollInterval, activationPollMinimum)):
			deactivation, err = client.GetDeactivation(ctx, edgeworkers.GetDeactivationRequest{
				EdgeWorkerID:   edgeworkerID,
				DeactivationID: deactivationID,
			})
			if err != nil {
				return nil, err
			}
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, ErrEdgeworkerDeactivationTimeout
			}
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil, ErrEdgeworkerDeactivationCancelled
			}
			return nil, fmt.Errorf("%v: %w", ErrEdgeworkerDeactivationContextTerminated, ctx.Err())
		}
	}
	return deactivation, nil
}

func filterActivationsByNetwork(acts []edgeworkers.Activation, net string) (activations []edgeworkers.Activation) {
	for _, act := range acts {
		if act.Network == net {
			activations = append(activations, act)
		}
	}
	return activations
}

func filterDeactivationsByNetwork(deacts []edgeworkers.Deactivation, net string) (deactivations []edgeworkers.Deactivation) {
	for _, deact := range deacts {
		if deact.Network == edgeworkers.ActivationNetwork(net) {
			deactivations = append(deactivations, deact)
		}
	}
	return deactivations
}

func sortActivationsByDate(activations []edgeworkers.Activation) []edgeworkers.Activation {
	sort.Slice(activations, func(i, j int) bool {
		timeLayout := time.RFC3339
		t1, err := time.Parse(timeLayout, activations[i].CreatedTime)
		if err != nil {
			panic(err)
		}
		t2, err := time.Parse(timeLayout, activations[j].CreatedTime)
		if err != nil {
			panic(err)
		}
		return t1.After(t2)
	})
	return activations
}

func sortDeactivationsByDate(deactivations []edgeworkers.Deactivation) []edgeworkers.Deactivation {
	sort.Slice(deactivations, func(i, j int) bool {
		timeLayout := time.RFC3339
		t1, err := time.Parse(timeLayout, deactivations[i].CreatedTime)
		if err != nil {
			panic(err)
		}
		t2, err := time.Parse(timeLayout, deactivations[j].CreatedTime)
		if err != nil {
			panic(err)
		}
		return t1.After(t2)
	})
	return deactivations
}
