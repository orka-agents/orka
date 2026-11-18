package patchverification

import (
	"encoding/json"
	"errors"
)

func validateApplicationReplicaEvidence(manifest Manifest, check Check, observation Observation, pending bool) error {
	app := observation.Application
	api := check.Application.Kubernetes
	if api == nil || api.APIVersion != applicationAppsVersion || api.Resource != applicationDeployments {
		if app.Replica != nil {
			return ErrEvidence
		}
		return nil
	}
	if app.Replica == nil && pending {
		return nil
	}
	profile := ApplicationReplicaTargetFor(manifest.ApplicationPolicy)
	if err := validateApplicationReplicaIdentity(profile, api, app); err != nil {
		return err
	}
	if err := validateApplicationReplicaDescendants(profile, observation); err != nil {
		return err
	}
	if check.Kind == Normal {
		return validateApplicationReplicaNormal(check, observation)
	}
	return nil
}

func validateApplicationReplicaIdentity(profile *ApplicationFixtureProfile, api *ApplicationKubernetesCheck, app *ApplicationObservation) error {
	proof := app.Replica
	if profile == nil || proof == nil || app.Kubernetes == nil ||
		api.Name != profile.ReplicaTarget.Name || proof.DeploymentUID != app.Kubernetes.UID ||
		!identifierPattern.MatchString(proof.DeploymentUID) ||
		proof.InitialReplicas != profile.ReplicaTarget.InitialReplicas || proof.MaxReplicas != profile.ReplicaTarget.MaxReplicas ||
		proof.DesiredReplicas < 0 || proof.DesiredReplicas > proof.MaxReplicas ||
		proof.ReadyReplicas < 0 || proof.ReadyReplicas > proof.DesiredReplicas ||
		int32(len(proof.Pods)) != proof.ReadyReplicas || len(proof.ReplicaSetUIDs) > 2 {
		return errors.New("replica evidence lacks the exact bounded Deployment and ready descendant identities")
	}
	return nil
}

func validateApplicationReplicaDescendants(profile *ApplicationFixtureProfile, observation Observation) error {
	app, proof := observation.Application, observation.Application.Replica
	sets := map[string]bool{}
	for _, uid := range proof.ReplicaSetUIDs {
		if !identifierPattern.MatchString(uid) || sets[uid] {
			return ErrBinding
		}
		sets[uid] = true
	}
	pods, containers := map[string]bool{}, map[string]bool{}
	for _, pod := range proof.Pods {
		if !applicationContainerValid(pod) || pod.JobUID != "" || pod.ImageID != profile.ImageID ||
			pod.Namespace != app.Target.Namespace || pod.NamespaceUID != app.Target.NamespaceUID ||
			pods[pod.PodUID] || containers[pod.ContainerID] || len(sets) == 0 ||
			pod.PodUID == app.Subject.PodUID || pod.PodUID == observation.PodUID || pod.PodUID == app.Artifact.Build.PodUID {
			return ErrBinding
		}
		pods[pod.PodUID], containers[pod.ContainerID] = true, true
	}
	return nil
}

func validateApplicationReplicaNormal(check Check, observation Observation) error {
	proof := observation.Application.Replica
	content, _ := json.Marshal(observation.Application.Kubernetes.Result)
	if string(content) == check.Healthy.Stdout &&
		(proof.ReadyReplicas == 0 || proof.DesiredReplicas != proof.ReadyReplicas ||
			proof.ReadyReplicas == proof.InitialReplicas) {
		return errors.New("normal scaling must demonstrate non-initial ready replicas, not a fixture assertion")
	}
	raw, _ := json.Marshal(proof.ReadyReplicas)
	expected, err := ApplicationAPIExpectation(ApplicationAPIValue{Exists: true, Found: true, Value: raw})
	if err != nil || observation.StdoutDigest != Digest([]byte(expected.Stdout)) || observation.StdoutBytes != len(expected.Stdout) {
		return errors.New("replica status bytes differ from the qualified ready descendant count")
	}
	return nil
}
