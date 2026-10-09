package devcli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nerds-run/go-agents/internal/devcli"
)

// The fixtures below are a deliberately correct manifest set. Every test case
// then MUTATES one line and asserts the check notices — the same
// break-it-and-watch-it-fail discipline the other gates in this repository were
// held to, except permanent. A gate nobody has seen fail is not evidence.

// Two runtime shapes are both correct, and the check must accept BOTH — it
// resolves a uid, it does not recognise a base image. distroless names the
// user; Debian creates it. An earlier version matched the literal `USER
// nonroot`, so moving to Debian broke the gate without breaking the image.
const validDockerfile = `FROM debian:trixie-slim
RUN groupadd --gid 65532 nonroot; \
    useradd --uid 65532 --gid 65532 --no-create-home --shell /usr/sbin/nologin nonroot
USER 65532:65532
COPY --from=build /out/server /usr/local/bin/server
ENTRYPOINT ["/usr/local/bin/server"]
`

const distrolessDockerfile = `FROM gcr.io/distroless/static-debian12:nonroot
USER nonroot:nonroot
COPY --from=build /out/server /server
ENTRYPOINT ["/server"]
`

const validDeployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: go-agents
spec:
  replicas: 2
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
  template:
    spec:
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
        - maxSkew: 1
          topologyKey: kubernetes.io/hostname
          whenUnsatisfiable: DoNotSchedule
          nodeTaintsPolicy: Honor
          matchLabelKeys:
            - pod-template-hash
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
      containers:
        - name: server
          image: go-agents:dev
          ports:
            - name: http
              containerPort: 8080
            - name: admin
              containerPort: 9090
          env:
            - name: GO_AGENTS_HTTP_ADDR
              value: :8080
            - name: GO_AGENTS_ADMIN_ADDR
              value: :9090
            - name: GO_AGENTS_API_TOKEN
              valueFrom:
                secretKeyRef:
                  name: go-agents-api-token
                  key: token
          resources:
            requests:
              cpu: 50m
              memory: 64Mi
            limits:
              memory: 128Mi
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          startupProbe:
            httpGet:
              path: /healthz
              port: http
          livenessProbe:
            httpGet:
              path: /healthz
              port: http
          readinessProbe:
            httpGet:
              path: /readyz
              port: http
`

const validService = `apiVersion: v1
kind: Service
metadata:
  name: go-agents
spec:
  type: ClusterIP
  ports:
    - name: http
      port: 80
      targetPort: http
`

// writeFixture materialises a repository root holding a Dockerfile and
// infrastructure/local/k8s, applying mutate to the deployment and service text first.
func writeFixture(t *testing.T, dockerfile, deployment, service string) string {
	t.Helper()
	root := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(root, "Dockerfile"), []byte(dockerfile), 0o600))
	// Derived from the constant, never spelled again: the last move left the
	// fixture writing to a directory the check no longer reads, and every case
	// failed with "no such file or directory" instead of on its own assertion.
	dir := filepath.Join(root, filepath.FromSlash(devcli.ManifestDir()))
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "deployment.yaml"), []byte(deployment), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "service.yaml"), []byte(service), 0o600))
	return root
}

func TestCheckManifestsAcceptsACorrectSet(t *testing.T) {
	t.Parallel()

	findings, err := devcli.CheckManifests(writeFixture(t, validDockerfile, validDeployment, validService))
	require.NoError(t, err)
	assert.Empty(t, findings, "the fixture is the shape infrastructure/local/k8s is supposed to have")
}

// TestCheckManifestsCatchesMutations is the mutation test. Each entry breaks
// exactly one invariant; if a row stops failing, the gate has stopped guarding
// that line.
func TestCheckManifestsCatchesMutations(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		old, new string
		wantSub  string
	}{
		"liveness probe moved to the readiness path": {
			// The expensive one: a slow store then restarts every pod,
			// turning a degradation into an outage.
			old: `          livenessProbe:
            httpGet:
              path: /healthz`,
			new: `          livenessProbe:
            httpGet:
              path: /readyz`,
			wantSub: `livenessProbe targets "/readyz"`,
		},
		"readiness probe moved to the liveness path": {
			old: `          readinessProbe:
            httpGet:
              path: /readyz`,
			new: `          readinessProbe:
            httpGet:
              path: /healthz`,
			wantSub: `readinessProbe targets "/healthz"`,
		},
		"readiness probe removed entirely": {
			old: `          readinessProbe:
            httpGet:
              path: /readyz
              port: http
`,
			new:     "",
			wantSub: "readinessProbe is missing",
		},
		"startup probe removed": {
			old: `          startupProbe:
            httpGet:
              path: /healthz
              port: http
`,
			new:     "",
			wantSub: "startupProbe is missing",
		},
		"probe pointed at the admin port": {
			old: `          readinessProbe:
            httpGet:
              path: /readyz
              port: http`,
			new: `          readinessProbe:
            httpGet:
              path: /readyz
              port: admin`,
			wantSub: `readinessProbe targets port "admin"`,
		},
		"http containerPort drifts from the binary default": {
			old:     "containerPort: 8080",
			new:     "containerPort: 8081",
			wantSub: "containerPort http is 8081",
		},
		"admin containerPort drifts from the binary default": {
			old:     "containerPort: 9090",
			new:     "containerPort: 9099",
			wantSub: "containerPort admin is 9099",
		},
		"env address disagrees with the containerPort": {
			old:     "value: :8080",
			new:     "value: :3000",
			wantSub: `GO_AGENTS_HTTP_ADDR is ":3000" but containerPort says 8080`,
		},
		"api token inlined as a literal": {
			old: `            - name: GO_AGENTS_API_TOKEN
              valueFrom:
                secretKeyRef:
                  name: go-agents-api-token
                  key: token`,
			new: `            - name: GO_AGENTS_API_TOKEN
              value: not-a-real-token`,
			wantSub: "carries a literal value",
		},
		"runAsUser drifts from the distroless uid": {
			old:     "runAsUser: 65532",
			new:     "runAsUser: 1000",
			wantSub: "runAsUser must be 65532",
		},
		"runAsNonRoot disabled": {
			old:     "runAsNonRoot: true",
			new:     "runAsNonRoot: false",
			wantSub: "runAsNonRoot must be true",
		},
		"privilege escalation allowed": {
			old:     "allowPrivilegeEscalation: false",
			new:     "allowPrivilegeEscalation: true",
			wantSub: "allowPrivilegeEscalation must be false",
		},
		"writable root filesystem": {
			old:     "readOnlyRootFilesystem: true",
			new:     "readOnlyRootFilesystem: false",
			wantSub: "readOnlyRootFilesystem must be true",
		},
		"capabilities no longer dropped": {
			old:     `drop: ["ALL"]`,
			new:     `drop: ["NET_RAW"]`,
			wantSub: "capabilities.drop must contain ALL",
		},
		"rolling update gives up availability": {
			old:     "maxUnavailable: 0",
			new:     "maxUnavailable: 1",
			wantSub: `maxUnavailable is "1"`,
		},
		"strategy switched to Recreate": {
			old:     "type: RollingUpdate",
			new:     "type: Recreate",
			wantSub: "progressive rollout requires RollingUpdate",
		},
		"single replica cannot surge": {
			old:     "replicas: 2",
			new:     "replicas: 1",
			wantSub: "replicas must be at least 2",
		},
		"grace period shorter than the drain window": {
			old:     "terminationGracePeriodSeconds: 30",
			new:     "terminationGracePeriodSeconds: 5",
			wantSub: "terminationGracePeriodSeconds must exceed",
		},
		"cpu request dropped": {
			old:     "cpu: 50m",
			new:     "",
			wantSub: "resources.requests must set both cpu and memory",
		},
		"memory limit dropped": {
			old: `            limits:
              memory: 128Mi`,
			new:     "",
			wantSub: "resources.limits.memory must be set",
		},
		"spread downgraded to a scheduler preference": {
			old:     "whenUnsatisfiable: DoNotSchedule",
			new:     "whenUnsatisfiable: ScheduleAnyway",
			wantSub: "ScheduleAnyway is a score, not a guarantee",
		},
		"lost nodes still count as topology domains": {
			old:     "          nodeTaintsPolicy: Honor\n",
			new:     "",
			wantSub: "nodeTaintsPolicy must be Honor",
		},
		"rolling update can finish skewed": {
			old:     "          matchLabelKeys:\n            - pod-template-hash\n",
			new:     "",
			wantSub: "matchLabelKeys must include pod-template-hash",
		},
		"spread constraint removed entirely": {
			old: `      topologySpreadConstraints:
        - maxSkew: 1
          topologyKey: kubernetes.io/hostname
          whenUnsatisfiable: DoNotSchedule
          nodeTaintsPolicy: Honor
          matchLabelKeys:
            - pod-template-hash
`,
			new:     "",
			wantSub: "no topologySpreadConstraints",
		},
		"image tag made mutable": {
			old:     "image: go-agents:dev",
			new:     "image: go-agents:latest",
			wantSub: "is unpinned",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			mutated := strings.Replace(validDeployment, tc.old, tc.new, 1)
			require.NotEqual(t, validDeployment, mutated, "the mutation did not apply; the fixture text moved")

			findings, err := devcli.CheckManifests(writeFixture(t, validDockerfile, mutated, validService))
			require.NoError(t, err)
			require.NotEmpty(t, findings, "the mutation went unnoticed")

			assert.Truef(t, containsSub(findings, tc.wantSub),
				"no finding mentioned %q; got %v", tc.wantSub, findings)
		})
	}
}

// TestCheckManifestsRejectsAdminPortOnTheService is the security boundary the
// whole two-listener split rests on, so it gets a test of its own rather than
// a row in the table.
func TestCheckManifestsRejectsAdminPortOnTheService(t *testing.T) {
	t.Parallel()

	for name, svc := range map[string]string{
		"by port number": strings.Replace(validService,
			`    - name: http
      port: 80
      targetPort: http`,
			`    - name: http
      port: 80
      targetPort: http
    - name: metrics
      port: 9090
      targetPort: admin`, 1),
		"by target name": strings.Replace(validService, "targetPort: http", "targetPort: admin", 1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			findings, err := devcli.CheckManifests(writeFixture(t, validDockerfile, validDeployment, svc))
			require.NoError(t, err)
			assert.Truef(t, containsSub(findings, "exposes the admin listener"),
				"a Service publishing the admin port must fail; got %v", findings)
		})
	}
}

// TestCheckManifestsResolvesTheImageUID covers every way the uid can be
// established, because the check has to work across base images rather than
// recognise one convention.
func TestCheckManifestsResolvesTheImageUID(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		dockerfile string
		wantSub    string // "" means the set must be accepted
	}{
		"debian, numeric USER":     {dockerfile: validDockerfile},
		"distroless, USER nonroot": {dockerfile: distrolessDockerfile},
		"named user created by useradd": {dockerfile: `FROM debian:trixie-slim
RUN useradd --uid 65532 --no-create-home nonroot
USER nonroot
`},
		"no USER at all, so it runs as root": {
			dockerfile: "FROM debian:trixie-slim\nCOPY --from=build /out/server /server\n",
			wantSub:    "image runs as uid 0",
		},
		"numeric USER disagreeing with the manifest": {
			dockerfile: "FROM debian:trixie-slim\nRUN useradd --uid 1000 --no-create-home app\nUSER 1000:1000\n",
			wantSub:    "image runs as uid 1000",
		},
		"numeric USER with no passwd entry for that uid": {
			dockerfile: "FROM debian:trixie-slim\nUSER 1000:1000\n",
			wantSub:    "cannot resolve the uid",
		},
		"a build-stage useradd is not attributed to a different runtime user": {
			dockerfile: `FROM debian:trixie-slim AS build
RUN useradd --uid 999 --no-create-home builder
FROM debian:trixie-slim
RUN useradd --uid 1001 --no-create-home app
USER app
`,
			wantSub: "image runs as uid 1001",
		},
		"useradd line ends with a bare trailing semicolon": {
			dockerfile: "FROM debian:trixie-slim\nRUN useradd --uid 1001 app;\nUSER app\n",
			wantSub:    "image runs as uid 1001",
		},
		"named user the Dockerfile never creates": {
			dockerfile: "FROM debian:trixie-slim\nUSER appuser\n",
			wantSub:    "cannot resolve the uid",
		},
		"useradd for a DIFFERENT uid than the manifest": {
			dockerfile: "FROM debian:trixie-slim\nRUN useradd --uid 1001 app\nUSER app\n",
			wantSub:    "image runs as uid 1001",
		},
		"a later USER root undoes an earlier drop": {
			dockerfile: "FROM debian:trixie-slim\nUSER 65532:65532\nUSER 0\n",
			wantSub:    "image runs as uid 0",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			findings, err := devcli.CheckManifests(writeFixture(t, tc.dockerfile, validDeployment, validService))
			require.NoError(t, err)
			if tc.wantSub == "" {
				assert.Empty(t, findings)
				return
			}
			assert.Truef(t, containsSub(findings, tc.wantSub),
				"no finding mentioned %q; got %v", tc.wantSub, findings)
		})
	}
}

// TestCheckManifestsFailsOnAnEmptyDirectory: a check that passes over nothing
// is the vacuous green this repository has already been bitten by once.
func TestCheckManifestsFailsOnAnEmptyDirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "Dockerfile"), []byte(validDockerfile), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, filepath.FromSlash(devcli.ManifestDir())), 0o750))

	findings, err := devcli.CheckManifests(root)
	require.NoError(t, err)
	assert.True(t, containsSub(findings, "no Deployment found"), findings)
}

// TestCheckManifestsReportsAParseErrorAsAnError, not as a missing Deployment.
// Reporting malformed YAML as "no Deployment found" sends you looking for a
// file that is right there.
func TestCheckManifestsReportsAParseError(t *testing.T) {
	t.Parallel()

	broken := strings.Replace(validDeployment, "  replicas: 2", "  replicas: [unclosed", 1)
	_, err := devcli.CheckManifests(writeFixture(t, validDockerfile, broken, validService))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deployment.yaml")
}

func TestManifestDirIsTheCommittedLocation(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "infrastructure/local/k8s", devcli.ManifestDir())
}

func containsSub(findings []devcli.ManifestFinding, sub string) bool {
	for _, f := range findings {
		if strings.Contains(f.String(), sub) {
			return true
		}
	}
	return false
}

// TestUnpinnedImage covers the reference forms the naive "contains a colon"
// test got wrong. The registry-port cases are the reason it exists: a colon in
// the HOST is not a tag, and the old check passed `registry:5000/app` — which
// resolves to latest — as pinned.
func TestUnpinnedImage(t *testing.T) {
	t.Parallel()

	pinned := []string{
		"go-agents:dev",
		"gcr.io/distroless/static-debian12:nonroot",
		"registry:5000/app:v1.2.3",
		"registry.example.com:8443/team/app:2026-09-04",
		"app@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"registry:5000/app@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	mutable := map[string]string{
		"app":                             "no tag",
		"app:latest":                      "latest",
		"registry:5000/app":               "a registry PORT is not a tag",
		"registry.example.com:8443/a":     "same, with a longer host",
		"registry:5000/app:latest":        "port AND latest",
		"gcr.io/distroless/static:latest": "latest behind a registry",
	}

	for _, img := range pinned {
		assert.Emptyf(t, devcli.UnpinnedImage(img), "%q must count as pinned", img)
	}
	for img, why := range mutable {
		assert.NotEmptyf(t, devcli.UnpinnedImage(img), "%q must be rejected (%s)", img, why)
	}
}
