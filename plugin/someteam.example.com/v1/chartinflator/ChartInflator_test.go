// +build notravis

// Copyright 2019 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0

// Disabled on travis, because don't want to install helm on travis.

package main_test

import (
	"fmt"
	"regexp"
	"testing"

	kusttest_test "sigs.k8s.io/kustomize/api/testutils/kusttest"
)

const expectedResourcesTemplate = `
apiVersion: v1
data:
  rcon-password: Q0hBTkdFTUUh
kind: Secret
metadata:
  labels:
    app: release-name-minecraft
    chart: minecraft-SOMEVERSION
    heritage: %s
    release: release-name
  name: release-name-minecraft
type: Opaque
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  annotations:
    volume.alpha.kubernetes.io/storage-class: default
  labels:
    app: release-name-minecraft
    chart: minecraft-SOMEVERSION
    heritage: %s
    release: release-name
  name: release-name-minecraft-datadir
spec:
  accessModes:
  - ReadWriteOnce
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: Service
metadata:
  labels:
    app: release-name-minecraft
    chart: minecraft-SOMEVERSION
    heritage: %s
    release: release-name
  name: release-name-minecraft
spec:
  ports:
  - name: minecraft
    port: 25565
    protocol: TCP
    targetPort: minecraft
  selector:
    app: release-name-minecraft
  type: LoadBalancer
`

func expectedResources(serviceName string) string {
	return fmt.Sprintf(expectedResourcesTemplate, serviceName, serviceName, serviceName)
}

// This test requires having "helmV2" (presumably helm V2 series) on the PATH.
//
// Download and inflate the chart, and check that
// in for the test.
func TestHelmV2ChartInflator(t *testing.T) {
	th := kusttest_test.MakeEnhancedHarness(t).
		PrepExecPlugin("someteam.example.com", "v1", "ChartInflator")
	defer th.Reset()

	m := th.LoadAndRunGenerator(`
apiVersion: someteam.example.com/v1
kind: ChartInflator
metadata:
  name: notImportantHere
chartName: minecraft
chartVersion: 1.2.0
helmBin: helmV2
`)

	chartName := regexp.MustCompile("chart: minecraft-[0-9.]+")
	th.AssertActualEqualsExpectedWithTweak(m,
		func(x []byte) []byte {
			return chartName.ReplaceAll(x, []byte("chart: minecraft-SOMEVERSION"))
		}, expectedResources("Tiller"))
}

// This test requires having "helmV3" (presumably helm V3 series) on the PATH.
//
func TestHelmV3ChartInflator(t *testing.T) {
	th := kusttest_test.MakeEnhancedHarness(t).
		PrepExecPlugin("someteam.example.com", "v1", "ChartInflator")
	defer th.Reset()

	m := th.LoadAndRunGenerator(`
apiVersion: someteam.example.com/v1
kind: ChartInflator
metadata:
  name: notImportantHere
chartRepo: https://kubernetes-charts.storage.googleapis.com/
chartName: minecraft
chartVersion: 1.2.0
helmBin: helmV3
`)

	chartName := regexp.MustCompile("chart: minecraft-[0-9.]+")
	th.AssertActualEqualsExpectedWithTweak(m,
		func(x []byte) []byte {
			return chartName.ReplaceAll(x, []byte("chart: minecraft-SOMEVERSION"))
		}, expectedResources("Helm"))
}
