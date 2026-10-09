package devcli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nerds-run/go-agents/internal/config"
)

// Kubernetes manifests make claims that nothing else in this repository can
// check. A probe pointed at the wrong port is a readiness gate that never
// passes; a securityContext that says uid 65532 while the image dropped its
// nonroot base is a pod that will not start; a Service that grows an "admin"
// port publishes /debug/pprof to anything that can resolve the name. All three
// are YAML-only mistakes: they compile, they lint, they apply, and they fail in
// the cluster.
//
// CheckManifests turns those claims into assertions against the code they
// describe — the listen addresses come from internal/config, the uid comes from
// the Dockerfile — so drifting one without the other fails the build.

// manifestDir is the directory of manifests this check reads.
const manifestDir = "infrastructure/local/k8s"

// nonrootUID is the uid this repository's runtime images run as, 65532
// either way: gcr.io/distroless/static-debian12:nonroot fixes it under the
// name `nonroot`, and the Debian base creates it explicitly and selects it
// numerically. The manifest must spell the number because the kubelet
// enforces runAsNonRoot without being able to resolve a name inside the
// image.
const nonrootUID = 65532

// workloadName is the Deployment (and Service) this check expects to find.
const workloadName = "go-agents"

// Object kinds and filenames spelled once, so a typo in a switch arm cannot
// silently skip a whole class of object.
const (
	kindDeployment = "Deployment"
	kindService    = "Service"
	dockerfile     = "Dockerfile"
)

// ManifestDir returns the repository-relative directory this check reads.
func ManifestDir() string { return manifestDir }

// ManifestFinding is one broken invariant, named so the failure message says
// which file and which object rather than "manifests are wrong".
type ManifestFinding struct {
	File    string
	Kind    string
	Name    string
	Problem string
}

func (f ManifestFinding) String() string {
	return fmt.Sprintf("%s (%s/%s): %s", f.File, f.Kind, f.Name, f.Problem)
}

// The subset of the Kubernetes schema this check reads. Typed rather than
// map[string]any so a renamed field is a compile error in the check itself.
type (
	manifestDoc struct {
		Kind     string       `yaml:"kind"`
		Metadata objectMeta   `yaml:"metadata"`
		Spec     manifestSpec `yaml:"spec"`
	}

	objectMeta struct {
		Name        string            `yaml:"name"`
		Labels      map[string]string `yaml:"labels"`
		Annotations map[string]string `yaml:"annotations"`
	}

	manifestSpec struct {
		// Deployment
		Replicas *int         `yaml:"replicas"`
		Strategy strategySpec `yaml:"strategy"`
		Template podTemplate  `yaml:"template"`
		// Service
		Type  string        `yaml:"type"`
		Ports []servicePort `yaml:"ports"`
	}

	strategySpec struct {
		Type          string `yaml:"type"`
		RollingUpdate struct {
			MaxSurge       yaml.Node `yaml:"maxSurge"`
			MaxUnavailable yaml.Node `yaml:"maxUnavailable"`
		} `yaml:"rollingUpdate"`
	}

	servicePort struct {
		Name       string    `yaml:"name"`
		Port       int       `yaml:"port"`
		TargetPort yaml.Node `yaml:"targetPort"`
	}

	podTemplate struct {
		Metadata objectMeta `yaml:"metadata"`
		Spec     podSpec    `yaml:"spec"`
	}

	podSpec struct {
		TerminationGracePeriodSeconds *int               `yaml:"terminationGracePeriodSeconds"`
		SecurityContext               podSecurity        `yaml:"securityContext"`
		TopologySpread                []spreadConstraint `yaml:"topologySpreadConstraints"`
		Containers                    []containerSpec    `yaml:"containers"`
	}

	spreadConstraint struct {
		MaxSkew           int      `yaml:"maxSkew"`
		TopologyKey       string   `yaml:"topologyKey"`
		WhenUnsatisfiable string   `yaml:"whenUnsatisfiable"`
		NodeTaintsPolicy  string   `yaml:"nodeTaintsPolicy"`
		MatchLabelKeys    []string `yaml:"matchLabelKeys"`
	}

	podSecurity struct {
		RunAsNonRoot *bool `yaml:"runAsNonRoot"`
		RunAsUser    *int  `yaml:"runAsUser"`
		RunAsGroup   *int  `yaml:"runAsGroup"`
	}

	containerSpec struct {
		Name            string            `yaml:"name"`
		Image           string            `yaml:"image"`
		Ports           []containerPort   `yaml:"ports"`
		Env             []envVar          `yaml:"env"`
		Resources       resourceSpec      `yaml:"resources"`
		SecurityContext containerSecurity `yaml:"securityContext"`
		StartupProbe    *probe            `yaml:"startupProbe"`
		LivenessProbe   *probe            `yaml:"livenessProbe"`
		ReadinessProbe  *probe            `yaml:"readinessProbe"`
	}

	containerPort struct {
		Name          string `yaml:"name"`
		ContainerPort int    `yaml:"containerPort"`
	}

	envVar struct {
		Name      string    `yaml:"name"`
		Value     *string   `yaml:"value"`
		ValueFrom yaml.Node `yaml:"valueFrom"`
	}

	resourceSpec struct {
		Requests map[string]string `yaml:"requests"`
		Limits   map[string]string `yaml:"limits"`
	}

	containerSecurity struct {
		AllowPrivilegeEscalation *bool `yaml:"allowPrivilegeEscalation"`
		ReadOnlyRootFilesystem   *bool `yaml:"readOnlyRootFilesystem"`
		Capabilities             struct {
			Drop []string `yaml:"drop"`
		} `yaml:"capabilities"`
	}

	probe struct {
		HTTPGet struct {
			Path string    `yaml:"path"`
			Port yaml.Node `yaml:"port"`
		} `yaml:"httpGet"`
	}
)

// loadedDoc pairs a parsed document with the file it came from.
type loadedDoc struct {
	file string
	doc  manifestDoc
}

// portOf extracts the port number from a listen address such as ":8080".
func portOf(addr string) (int, error) {
	_, p, ok := strings.Cut(addr, ":")
	if !ok {
		return 0, fmt.Errorf("address %q has no port", addr)
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return 0, fmt.Errorf("address %q has a non-numeric port: %w", addr, err)
	}
	return n, nil
}

// nodeValue renders a scalar YAML node, which is how ports and maxUnavailable
// arrive: both accept an int or a string and the distinction is not meaningful
// here, so comparing the rendered scalar avoids a union type.
//
// The field must be a yaml.Node VALUE, not a pointer: yaml.v3 refuses to
// unmarshal a scalar into *yaml.Node, and the resulting error surfaces as a
// whole-file parse failure.
func nodeValue(n yaml.Node) string { return n.Value }

// readManifests parses every YAML document under infrastructure/local/k8s.
func readManifests(root string) ([]loadedDoc, error) {
	dir := filepath.Join(root, manifestDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", manifestDir, err)
	}

	var out []loadedDoc
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || (!strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml")) {
			continue
		}
		// kustomization.yaml is not a Kubernetes object; parsing it as one
		// would produce an empty doc and a confusing "kind is empty" finding.
		if strings.HasPrefix(name, "kustomization.") {
			continue
		}
		raw, err := readUnderRoot(dir, name)
		if err != nil {
			return nil, err
		}
		rel := filepath.Join(manifestDir, name)
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		for {
			var doc manifestDoc
			derr := dec.Decode(&doc)
			if errors.Is(derr, io.EOF) {
				break
			}
			// A parse error must NOT be treated as end-of-stream. Doing so
			// made the check report "no Deployment found" for a file that was
			// merely mistyped — a gate that turns a malformed manifest into a
			// confusing pass-shaped failure teaches you to distrust it.
			if derr != nil {
				return nil, fmt.Errorf("parse %s: %w", rel, derr)
			}
			if doc.Kind == "" {
				continue
			}
			out = append(out, loadedDoc{file: rel, doc: doc})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].file < out[j].file })
	return out, nil
}

// CheckManifests validates infrastructure/local/k8s against the code it claims to deploy.
func CheckManifests(root string) ([]ManifestFinding, error) {
	docs, err := readManifests(root)
	if err != nil {
		return nil, err
	}

	httpPort, err := portOf(config.DefaultHTTPAddr)
	if err != nil {
		return nil, err
	}
	adminPort, err := portOf(config.DefaultAdminAddr)
	if err != nil {
		return nil, err
	}

	var findings []ManifestFinding
	var sawDeployment bool

	for _, d := range docs {
		switch d.doc.Kind {
		case kindDeployment:
			sawDeployment = true
			findings = append(findings, checkDeployment(d, httpPort, adminPort)...)
		case kindService:
			findings = append(findings, checkService(d, adminPort)...)
		default:
			// Namespace, PodDisruptionBudget and anything else carry no
			// invariant this check owns.
		}
	}

	if !sawDeployment {
		findings = append(findings, ManifestFinding{
			File: manifestDir, Kind: kindDeployment, Name: workloadName,
			Problem: "no Deployment found; the gate would pass over an empty directory",
		})
	}

	dockerfileFindings, err := checkDockerfileMatchesPodSecurity(root, docs)
	if err != nil {
		return nil, err
	}
	findings = append(findings, dockerfileFindings...)

	return findings, nil
}

func checkDeployment(d loadedDoc, httpPort, adminPort int) []ManifestFinding {
	var out []ManifestFinding
	add := func(format string, args ...any) {
		out = append(out, ManifestFinding{
			File: d.file, Kind: d.doc.Kind, Name: d.doc.Metadata.Name,
			Problem: fmt.Sprintf(format, args...),
		})
	}

	spec := d.doc.Spec

	// A single replica cannot roll without a gap, so maxUnavailable: 0 would
	// deadlock rather than protect anything.
	if spec.Replicas == nil || *spec.Replicas < 2 {
		add("replicas must be at least 2 for a surge-based rolling update to have anywhere to go")
	}
	if spec.Strategy.Type != "RollingUpdate" {
		add("strategy.type is %q; progressive rollout requires RollingUpdate", spec.Strategy.Type)
	}
	// The whole claim of a safe rollout: a new pod must become Ready before an
	// old one is removed. maxUnavailable > 0 gives that up silently.
	if v := nodeValue(spec.Strategy.RollingUpdate.MaxUnavailable); v != "0" {
		add("rollingUpdate.maxUnavailable is %q; must be 0 so a failing image stalls the rollout instead of shrinking capacity", v)
	}
	if nodeValue(spec.Strategy.RollingUpdate.MaxSurge) == "" {
		add("rollingUpdate.maxSurge is unset; with maxUnavailable 0 the rollout could never start")
	}

	pod := spec.Template.Spec
	minGrace := int(config.DefaultShutdownTimeout.Seconds())
	if pod.TerminationGracePeriodSeconds == nil || *pod.TerminationGracePeriodSeconds <= minGrace {
		add("terminationGracePeriodSeconds must exceed the %ds drain window (config.DefaultShutdownTimeout) or the kubelet SIGKILLs a draining process", minGrace)
	}

	out = append(out, checkPodSecurity(d, pod.SecurityContext)...)
	checkTopologySpread(pod.TopologySpread, add)

	if len(pod.Containers) == 0 {
		add("no containers")
		return out
	}
	for _, c := range pod.Containers {
		out = append(out, checkContainer(d, c, httpPort, adminPort)...)
	}
	return out
}

// checkTopologySpread enforces that the spread is a constraint rather than a
// preference. ScheduleAnyway is only a scheduler score, routinely outweighed:
// both replicas then land on one node, the PodDisruptionBudget protects
// nothing, and every other file here still claims the service survives losing
// a node. Observed in this repository's own kind cluster before it was fixed.
func checkTopologySpread(cs []spreadConstraint, add func(string, ...any)) {
	if len(cs) == 0 {
		add("no topologySpreadConstraints; two replicas may both land on one node")
		return
	}
	for _, c := range cs {
		if c.WhenUnsatisfiable != "DoNotSchedule" {
			add("topologySpreadConstraints[%s].whenUnsatisfiable is %q; ScheduleAnyway is a score, not a guarantee",
				c.TopologyKey, c.WhenUnsatisfiable)
		}
		// Without this, DoNotSchedule blocks the replacement pod when a node
		// goes down — trading an availability guarantee for an outage.
		if c.NodeTaintsPolicy != "Honor" {
			add("topologySpreadConstraints[%s].nodeTaintsPolicy must be Honor, or a lost node still counts as a domain and the replacement pod cannot schedule",
				c.TopologyKey)
		}
		// The skew is computed against the pods that exist at scheduling time,
		// so an outgoing ReplicaSet's pods make room for a new pod on a node
		// that already has one — and a rolling update finishes with both
		// replicas together. Observed twice in this repository's kind cluster.
		if !slices.Contains(c.MatchLabelKeys, "pod-template-hash") {
			add("topologySpreadConstraints[%s].matchLabelKeys must include pod-template-hash, or a rolling update can finish with every replica on one node",
				c.TopologyKey)
		}
	}
}

func checkPodSecurity(d loadedDoc, sc podSecurity) []ManifestFinding {
	var out []ManifestFinding
	add := func(format string, args ...any) {
		out = append(out, ManifestFinding{
			File: d.file, Kind: d.doc.Kind, Name: d.doc.Metadata.Name,
			Problem: fmt.Sprintf(format, args...),
		})
	}

	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		add("securityContext.runAsNonRoot must be true")
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != nonrootUID {
		add("securityContext.runAsUser must be %d, the uid this repository's runtime image runs as", nonrootUID)
	}
	if sc.RunAsGroup == nil || *sc.RunAsGroup != nonrootUID {
		add("securityContext.runAsGroup must be %d, the uid this repository's runtime image runs as", nonrootUID)
	}
	return out
}

//nolint:gocognit // one linear list of independent container invariants; splitting it would hide the checklist.
func checkContainer(d loadedDoc, c containerSpec, httpPort, adminPort int) []ManifestFinding {
	var out []ManifestFinding
	add := func(format string, args ...any) {
		out = append(out, ManifestFinding{
			File: d.file, Kind: d.doc.Kind, Name: d.doc.Metadata.Name + "/" + c.Name,
			Problem: fmt.Sprintf(format, args...),
		})
	}

	ports := map[string]int{}
	for _, p := range c.Ports {
		ports[p.Name] = p.ContainerPort
	}
	if ports["http"] != httpPort {
		add("containerPort http is %d; the binary defaults to %d (config.DefaultHTTPAddr)", ports["http"], httpPort)
	}
	if ports["admin"] != adminPort {
		add("containerPort admin is %d; the binary defaults to %d (config.DefaultAdminAddr)", ports["admin"], adminPort)
	}

	env := map[string]envVar{}
	for _, e := range c.Env {
		env[e.Name] = e
	}
	// The env var and the containerPort are two independent statements of the
	// same fact. When they disagree the probe hits a closed port and the pod
	// never becomes Ready, with nothing in the logs to say why.
	assertAddr := func(key string, want int) {
		e, ok := env[key]
		if !ok || e.Value == nil {
			return // unset means the binary's own default, which is `want`.
		}
		got, err := portOf(*e.Value)
		if err != nil {
			add("%s is %q, which has no parseable port", key, *e.Value)
			return
		}
		if got != want {
			add("%s is %q but containerPort says %d", key, *e.Value, want)
		}
	}
	assertAddr("GO_AGENTS_HTTP_ADDR", ports["http"])
	assertAddr("GO_AGENTS_ADMIN_ADDR", ports["admin"])

	// AGENTS.md rule 1, made structural: a token spelled inline here is a
	// secret in git, and the gitleaks allowlist for it would be permanent.
	if tok, ok := env["GO_AGENTS_API_TOKEN"]; ok {
		if tok.Value != nil {
			add("GO_AGENTS_API_TOKEN carries a literal value; it must come from a secretKeyRef")
		} else if tok.ValueFrom.Kind == 0 {
			add("GO_AGENTS_API_TOKEN has neither a value nor a valueFrom")
		}
	}

	checkProbes(c, add)

	if c.Resources.Requests["cpu"] == "" || c.Resources.Requests["memory"] == "" {
		add("resources.requests must set both cpu and memory, or the scheduler cannot place the pod deliberately")
	}
	// Memory only. A CPU limit throttles latency-sensitive request handling
	// for no isolation the request does not already provide.
	if c.Resources.Limits["memory"] == "" {
		add("resources.limits.memory must be set so a leak is an OOM rather than a node-wide problem")
	}

	sc := c.SecurityContext
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		add("securityContext.allowPrivilegeEscalation must be false")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		add("securityContext.readOnlyRootFilesystem must be true; the binary writes nothing")
	}
	if !slices.Contains(sc.Capabilities.Drop, "ALL") {
		add("securityContext.capabilities.drop must contain ALL")
	}
	if why := UnpinnedImage(c.Image); why != "" {
		add("image %q is unpinned (%s); a mutable reference makes a rollback point at the same bits", c.Image, why)
	}
	return out
}

// UnpinnedImage reports why a reference is mutable, or "" when it is pinned.
//
// The naive test — "contains a colon, so it has a tag" — is wrong, and wrong in
// the direction that passes bad input: a colon also appears in a REGISTRY PORT.
// `registry:5000/app` has one and no tag at all, so it resolved to `latest`
// while satisfying the check that exists to forbid exactly that. The repo does
// not use a registry today, which is precisely why the hole would have gone
// unnoticed until the day it mattered.
//
// A digest (`app@sha256:...`) is the strongest pin there is and must pass.
func UnpinnedImage(image string) string {
	// Digest wins outright: it names the bits, not a label pointing at them.
	if name, digest, ok := strings.Cut(image, "@"); ok && name != "" && digest != "" {
		return ""
	}
	// The tag, if any, lives in the LAST path segment. Anything before the
	// final "/" is registry and namespace, where a colon is a port.
	last := image
	if i := strings.LastIndex(image, "/"); i >= 0 {
		last = image[i+1:]
	}
	_, tag, ok := strings.Cut(last, ":")
	switch {
	case !ok || tag == "":
		return "no tag, so it resolves to latest"
	case tag == "latest":
		return "the latest tag moves"
	default:
		return ""
	}
}

// checkProbes enforces the division of labour between the three probes. The
// expensive mistake is a liveness probe on /readyz: a slow dependency then
// restarts every pod, converting a degradation into an outage.
func checkProbes(c containerSpec, add func(string, ...any)) {
	const (
		healthPath = "/healthz"
		readyPath  = "/readyz"
	)

	requireProbe := func(name string, p *probe, wantPath string) {
		if p == nil {
			add("%s is missing; the kubelet cannot act on health it is never told about", name)
			return
		}
		if p.HTTPGet.Path != wantPath {
			add("%s targets %q; it must target %q", name, p.HTTPGet.Path, wantPath)
		}
		// Probing the admin port would make readiness depend on the listener
		// the Service deliberately does not route.
		if port := nodeValue(p.HTTPGet.Port); port != "http" {
			add("%s targets port %q; probes belong on the published http port", name, port)
		}
	}

	requireProbe("startupProbe", c.StartupProbe, healthPath)
	requireProbe("livenessProbe", c.LivenessProbe, healthPath)
	requireProbe("readinessProbe", c.ReadinessProbe, readyPath)
}

func checkService(d loadedDoc, adminPort int) []ManifestFinding {
	var out []ManifestFinding
	for _, p := range d.doc.Spec.Ports {
		// The security boundary: a second listener only protects anything
		// while nothing routes to it. Prometheus scrapes the pod IP directly.
		if p.Port == adminPort || nodeValue(p.TargetPort) == "admin" || p.Name == "admin" {
			out = append(out, ManifestFinding{
				File: d.file, Kind: d.doc.Kind, Name: d.doc.Metadata.Name,
				Problem: fmt.Sprintf(
					"port %q exposes the admin listener (%d); that publishes /debug/pprof and /metrics to anything that can resolve this Service",
					p.Name, adminPort),
			})
		}
	}
	return out
}

// checkDockerfileMatchesPodSecurity ties the manifest's uid to the image that
// has to honour it. The manifest asserting 65532 while the image ships a root
// USER is a pod that CrashLoops on `runAsNonRoot`, and nothing else in this
// repository compares the two files.
//
// The check reads the UID rather than a base-image name on purpose. An earlier
// version matched the literal strings `USER nonroot` and `:nonroot`, which
// encoded one specific distroless convention — moving the runtime to Debian
// then broke the gate without breaking the image, which is the wrong way round.
func checkDockerfileMatchesPodSecurity(root string, docs []loadedDoc) ([]ManifestFinding, error) {
	raw, err := readUnderRoot(root, dockerfile)
	if err != nil {
		return nil, err
	}

	var want *int
	for _, d := range docs {
		if d.doc.Kind == kindDeployment && d.doc.Spec.Template.Spec.SecurityContext.RunAsUser != nil {
			want = d.doc.Spec.Template.Spec.SecurityContext.RunAsUser
		}
	}
	if want == nil {
		return nil, nil
	}

	got, how := dockerfileUID(string(raw))
	if got == nil {
		return []ManifestFinding{{
			File: dockerfile, Kind: dockerfile, Name: "runtime stage",
			Problem: fmt.Sprintf(
				"cannot resolve the uid the image runs as (%s), but %s asserts runAsUser %d",
				how, manifestDir, *want),
		}}, nil
	}
	if *got != *want {
		return []ManifestFinding{{
			File: dockerfile, Kind: dockerfile, Name: "runtime stage",
			Problem: fmt.Sprintf("image runs as uid %d (%s) but %s asserts runAsUser %d",
				*got, how, manifestDir, *want),
		}}, nil
	}
	return nil, nil
}

// userInstruction captures the argument of a USER instruction.
var userInstruction = regexp.MustCompile(`(?mi)^\s*USER\s+([^\s#]+)`)

// useraddUID captures the uid AND the username a useradd invocation creates,
// per line, so a uid can be attributed to the account it actually belongs to
// rather than to whichever useradd happens to appear first in the file. A
// multi-stage Dockerfile can create more than one account under more than
// one uid, and the username is the last whitespace-separated token on a
// useradd line by convention.
var useraddUID = regexp.MustCompile(`(?mi)^.*\buseradd\b.*?--uid[ =](\d+).*?([^\s;]+)\s*;?\s*$`)

// dockerfileUID resolves the uid the LAST USER instruction selects, plus a
// short description of how it was resolved, for the failure message.
//
// A named user is resolved through the `useradd --uid` line that created
// THAT SAME name, not just any `useradd --uid` in the file. A numeric USER is
// trusted only for uid 0 (root always has a passwd entry) or when some
// `useradd --uid` line created a passwd entry for that exact uid — Docker
// happily starts a container under a numeric uid nothing ever created, but
// os/user lookups and $HOME resolution fail against it at runtime. The
// distroless `:nonroot` convention fixes 65532. Anything else returns nil
// rather than a guess: guessing here would either pass a root image or fail
// a correct one, and both are worse than saying so.
func dockerfileUID(body string) (*int, string) {
	m := userInstruction.FindAllStringSubmatch(body, -1)
	if len(m) == 0 {
		zero := 0
		return &zero, "no USER instruction, so the image runs as root"
	}
	name, _, _ := strings.Cut(m[len(m)-1][1], ":")

	if n, err := strconv.Atoi(name); err == nil {
		if n == 0 {
			return &n, "numeric USER 0 (root, always has a passwd entry)"
		}
		for _, u := range useraddUID.FindAllStringSubmatch(body, -1) {
			if uid, err := strconv.Atoi(u[1]); err == nil && uid == n {
				return &n, fmt.Sprintf("numeric USER %d created by useradd --uid", n)
			}
		}
		return nil, fmt.Sprintf("USER %d with no useradd --uid %d, so it has no passwd entry", n, n)
	}
	for _, u := range useraddUID.FindAllStringSubmatch(body, -1) {
		if u[2] != name {
			continue
		}
		if n, err := strconv.Atoi(u[1]); err == nil {
			return &n, "USER " + name + " created by useradd --uid"
		}
	}
	if name == "nonroot" && strings.Contains(body, ":nonroot") {
		n := nonrootUID
		return &n, "USER nonroot on a distroless :nonroot base"
	}
	return nil, "USER " + name + " with no useradd --uid and no :nonroot base"
}
