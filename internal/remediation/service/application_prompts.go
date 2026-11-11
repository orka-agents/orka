package service

import (
	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/intake"
	"github.com/orka-agents/orka/internal/remediation/source"
)

const ApplicationChecksMarker = "ORKA_REMEDIATION_APPLICATION_CHECKS_V1"

func applicationChecksPrompt(report intake.Report, packet source.Packet, defaults pv.ApplicationPlan, policy *pv.ApplicationPolicy) string {
	return prompt(ApplicationChecksMarker,
		"Propose an application-runtime reproduction and real normal-use controls for this exact upstream commit. "+
			"The model-visible source packet is bounded; the builder receives the COMPLETE immutable upstream tree, "+
			"not just selected files. Do not copy or simulate the application's implementation in a check. "+
			"Return one JSON object with scope, gaps, requirements:[], files:[], services:[], application, and checks. "+
			"scope must cover the entire report. Declare unsupported required behavior in gaps; never silently narrow the report. "+
			"Do not return policy or applicationPolicy: operatorPolicy is frozen authority, not a model proposal. "+
			"application has version:1, build:{profile:\"go-package-v1\",package:\"./relative/main/package\",goVersion:\"...\"}, "+
			"runtime:{image:\"...\",imageID:\"...\",args:[],ports:[]}, resources:[], permissions:[], fixtures:[], syntheticSecrets:[]. "+
			"Copy the operatorDefaults build profile, Go version and exact runtime image/imageID. Choose only the actual "+
			"upstream main package, literal application arguments, admitted data resources and checks. "+
			"Builds are offline, vendored and CGO-disabled: no downloads, Dockerfiles, shells, generate/toolexec, custom build commands, "+
			"or substitute programs. The long-running non-root subject has writable /work and read-only /artifacts; "+
			"source files are not runtime fixtures. Arguments support only the fixed {{namespace}} substitution. "+
			"Resources must be named, namespaced, admitted JSON data objects. Without operatorPolicy, only ConfigMap, Service "+
			"and Ingress are supported. With policy, use only its exact resource GVKs/GVRs and privileges; CRDs are preinstalled "+
			"and identity/schema-fenced by the operator. Do not supply CRDs, Pods, RBAC objects, namespace, UID, ownerReferences, status, "+
			"or credential data. Names beginning orka- are reserved. Keep resource names unique across plan and check setup inputs. "+
			"Operator-admitted cluster-scoped CUSTOM GVRs are a subject-only startup read capability: get/list/watch only, "+
			"never cluster fixtures, actor actions, observation predicates, status writes or other mutations. "+
			"If the actual controller requires cluster mutations, declare the unmet requirement; never simulate it. "+
			"permissions are {apiGroup:\"...\",resources:[\"...\"],verbs:[\"...\"],resourceNames:[\"...\"]}, "+
			"strict subsets of operatorPolicy.subjectPermissions. Never request wildcards, unadmitted cluster access, impersonation, exec, "+
			"credential minting or privileges absent from the policy. Secret reads can cover only backend-generated synthetic "+
			"fixtures in the isolated candidate namespace, never protected observer/actor/receiver credentials. "+
			"Subject namespace-selection variables come ONLY from operatorPolicy.subjectEnvironment (at most eight approved names "+
			"whose exact value is {{namespace}}). The trusted launcher substitutes each side's own namespace. Do not return "+
			"model environment maps, arbitrary literals, EnvFrom, ValueFrom, credentials or loader/proxy/TLS overrides. "+
			"Operator subjectTLS entries provide fresh lab-only ca.crt, tls.crt and tls.key read-only at /subject-config/<ID>. "+
			"Runtime arguments may select that fixed directory. Do not propose TLS bundles, keys, certificate values, Secret refs "+
			"or mounts. The standard serviceaccount namespace file or approved namespace environment supplies the actual namespace. "+
			"Synthetic Secret recipes are {name:\"...\",keys:[{name:\"...\",markerID:\"...\"}]}; the trusted backend generates "+
			"fresh random values. You receive only references, never values. Do not invent, provide, compute or print Secret values. "+
			"fixtures are {id:\"...\",kind:\"marker\"|\"receiver\"|\"image\"|\"replica\",port:1234,profile:\"catalogue ID\",config:{}}. "+
			"marker emits a protected plaintext marker and EOF, not an HTTP response. receiver collects subject-originated traffic; "+
			"image requires an operatorPolicy.fixtures catalogue ID and only its admitted config keys, never an arbitrary image/command. "+
			"A catalogue replicaTarget is the ONLY path to a Deployment fixture. Select kind:replica, the exact catalogue profile, "+
			"and id equal to replicaTarget.name. Initial/max replicas and image/argv/security/runtimeClass are operator-fixed. "+
			"At most one replica target is allowed; port0 is valid for a non-network catalogue target. Replica fixtures cannot use "+
			"config or secretFiles. Never supply raw Deployment/HPA resources, PodSpecs/templates, Jobs or cross-namespace targets. "+
			"Subject /scale privileges apply only to the exact fixture name; HPA writes require live Deny admission enforcing "+
			"that target and finite replica limits, with additional quotas. Test data must stay within the profile, never bypass it. "+
			"A normal0-to1 control needs actor stimulus and actual subject reconciliation with bounded Deployment replica/ready "+
			"observations different from the operator's initial count; HPA existence or fabricated status alone is not scaling proof. "+
			"Image catalogue secretFiles select only named/key definitions in this plan's syntheticSecrets, exposed read-only at "+
			"/fixture-secrets/<ID>. Operator fixed argv may read those files. Do not add Secret refs/values/env/mounts yourself. "+
			"Admitted Services can select subject or fixture IDs with selector {\"orka.ai/application-target\":\"ID\"}. "+
			"The backend provides fixed service host aliases; general DNS/network egress is not authority. "+
			"Include 2-8 checks with at least one reproduction and one real normal-use check. Each has id, "+
			"kind:\"reproduction\"|\"normal\", timeoutSeconds, healthy:{exitCode:0,stdout:\"...\"}, failure:{exitCode:0,stdout:\"...\"}, "+
			"and application:{version:1,settleSeconds:1,resources:[],actions:[],kubernetes:{apiVersion:\"...\",resource:\"...\",name:\"...\",pointer:\"/status/...\",normalValue:\"<actual public healthy value for a non-replica normal check>\"}} "+
			"OR application:{version:1,actions:[],tcp:{target:\"...\",port:1234,markerID:\"...\",maxBytes:4096}}. "+
			"Never use command, http, stdin or lifecycle hooks. API pointers may inspect only /status and descendants, not "+
			"fixture-declared spec, metadata, logs or Secret data. Every API reproduction needs a normal control for the same "+
			"apiVersion/resource/status pointer; object names may differ. "+
			"Every non-replica normal API check additionally requires kubernetes.normalValue: the exact declared PUBLIC "+
			"healthy JSON value, matching healthy.stdout via exists:true, found:true and that value. Nonempty meaningful "+
			"values are required; null, whitespace, empty arrays/objects and vacuous nested values are invalid. Explicit false "+
			"and zero remain valid when they are the genuine non-initial healthy result; never invent true to satisfy a check. "+
			"The normal target must be a declared owned plan/check resource or a matching successful actor create/update, "+
			"and the frozen subject permissions must actually permit update/patch of that resource/status. "+
			"Success requires the SAME real target UID, changed resourceVersion AND changed reduced status value from the "+
			"authoritative fixture Create response or latest matching successful actor response—not a later GET or absent-target guess. "+
			"Do not return apiBaseline, initialAPI, observed UIDs, resourceVersions or digests as proposal fields; "+
			"only the trusted execution path supplies those observations. Replica/TCP normal controls keep their separate contracts. "+
			"actions use REAL named operatorPolicy.actors identities: {actorID:\"...\",verb:\"create\"|\"update\",resource:{...},expectedStatus:201|200|403}. "+
			"Create uses a fixed POST to an absent, durably receipted name; update uses a fixed PUT fenced by existing UID/resourceVersion. "+
			"Use 201 for allowed create, 200 for allowed update, or 403 for required denial; no arbitrary URLs or patch DSL. "+
			"The observer authenticates each actual actor, records each response, and never substitutes its own higher authority. "+
			"A 403 counts as authorization denial only with the actor's exact SelfSubjectAccessReview denial, not an admission or quota failure. "+
			"Choose actors matching the report's actual privileges; a privileged actor is not a substitute for an unprivileged reported subject. "+
			"Authorization outcomes alone do not prove reconciliation: include independent status/receiver observations and normal-use controls. "+
			"For subject-originated traffic use tcp:{mode:\"receive\",target:\"receiver fixture ID\",port:1234,markerID:\"synthetic key markerID\", "+
			"maxBytes:4096,windowSeconds:30,terminator:\"optional base64 frame terminator\",response:\"optional base64 fixed response\"}. "+
			"The trusted receiver is listening before actions execute, accepts only the exact live subject's address, and observes "+
			"the complete bounded window. EOF is the default frame terminator; setup failure, truncation or timeout is not absence proof. "+
			"Receive-mode checks need at least one actor stimulus. A healthy {\"connected\":false,\"markerMatched\":false} "+
			"may represent zero subject connections only after the complete proven listener window; normal healthy must be {\"connected\":true,\"markerMatched\":true}. "+
			"Receive-mode reproduction needs a positive normal subject control for the same receiver, port and marker. "+
			"Legacy pull checks use target:\"subject\", optional base64 request bytes and the same subject port/marker in the normal "+
			"control; normal healthy markerMatched must be true. Fixture-only observations do not prove subject behavior. "+
			"API expectations are JSON-encoded strings {\"exists\":true,\"found\":true,\"value\":...}; use "+
			"{\"exists\":false,\"found\":false} for absent objects or {\"exists\":true,\"found\":false} for absent pointers. "+
			"Propose exact values, not hashes: trusted freezing canonicalizes them into valueDigest/valueBytes before execution. "+
			"TCP expectations use exact canonical {\"connected\":true,\"markerMatched\":true} or the actual false values permitted by the mode. "+
			"All exit codes describe completed independent observation, not the subject's status. Healthy and failure must differ. "+
			"Do not treat logs, model claims, policy labels, missing evidence or a guessed response as proof. Original and candidate "+
			"run the same complete frozen plan. Unsupported environments must remain inconclusive, never source-harness substitutes.",
		struct {
			Report   intake.Report         `json:"report"`
			Source   source.Packet         `json:"source"`
			Defaults pv.ApplicationPlan    `json:"operatorDefaults"`
			Policy   *pv.ApplicationPolicy `json:"operatorPolicy,omitempty"`
		}{report, packet, defaults, policy})
}
