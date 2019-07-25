<p align="center"><img src="./service-reflector.svg" width="200"></p>

# Service-Reflector

Service-Reflector mirrors Kubernetes Services so that Pods running in one cluster can natively access Services from another cluster.

[![Build Status](https://travis-ci.org/squat/service-reflector.svg?branch=master)](https://travis-ci.org/squat/service-reflector)
[![Go Report Card](https://goreportcard.com/badge/github.com/squat/service-reflector)](https://goreportcard.com/report/github.com/squat/service-reflector)

## Overview

A key use for multi-cluster Kubernetes is to allow Pods and Services in one cluster to access those running in another cluster.
However, a Pod in cluster A has no Kubernetes-native way of discovering a Service in cluster B or determining its endpoints; instead it must be pointed explicitly at the Service's ClusterIP.
One solution to this issue is for an administrator to create a Service in cluster A to mirror the one in cluster B with Endpoints that point at the Service’s Pods.
Service-Reflector automates the mirroring of selected Services from one cluster to another to simplify running multi-cluster Kubernetes.

## How it works

Service-Reflector is comprised of two components: a Reflector and an Emitter.
By default, both components are run, though either can be disabled, via the `--reflector=false` and `--emitter=false` flags.

### Reflector

The Reflector is a controller that watches source APIs for Services to reflect and creates mirror Services and Endpoints in the host cluster.
One Reflector can watch many sources for Services, allowing a cluster to access Services from many different clusters.
The source APIs can be any Kubernetes-style API that allows listing and watching Services and Endpoints, for example:
* a Kubernetes apiserver, specified with the `--reflector.source-kubeconfig` flag; or
* an Emitter, specified with the `--reflector.source-api` flag.

### Emitter

In order allow a Reflector in cluster A to find Services in cluster B, we must point the Reflector at a source API, namely cluster B's Kubernetes apiserver.
Doing so requires configuring the Reflector with a Kubeconfig or ServiceAccount token from cluster B.
To simplify deployment, Service-Reflector provides an Emitter, a Kubernetes-style apiserver that implements only the list and watch verbs for Services and Endpoints.
The only details a Reflector needs to access an Emitter's API is the Emitter's URL.
Furthermore, the Emitter allows an administrator to specify a selector to limit which Services are exposed to the Reflector; this is done via the `--emitter.selector` flag.

## Installing on Kubernetes

### Step 1: connect Kubernetes clusters

Start by connecting two or more Kubernetes clusters with the desired multi-cluster solution, e.g. [Kilo](https://github.com/squat/kilo).
The Service CIDR and Pod CIDR of each cluster must be routable from the others.

### Step 2: install Service-Reflector!

Service-Reflector can be installed on any Kubernetes cluster by deploying a Deployment:

```shell
kubectl apply -f https://raw.githubusercontent.com/squat/service-reflector/master/manifests/service-reflector.yaml
```

### Step 3: specify source APIs

In order for the Reflector to find Services to mirror, it must be configured with a source API.
The following snippet could be used to configure Service-Reflectors in two clusters:

```shell
# Register the emitter in cluster1 as a source of the reflector in cluster2.
cat <<EOF | kubectl --kubeconfig $KUBECONFIG2 apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: service-reflector
data:
  source-api: http://$(kubectl --kubeconfig $KUBECONFIG1 get service service-reflector -o jsonpath='{.spec.clusterIP}'):8080
EOF
# Restart the reflector.
kubectl --kubeconfig $KUBECONFIG2 delete pod --selector app.kubernetes.io/name=service-reflector
# Register the emitter in cluster2 as a source of the reflector in cluster1.
cat <<EOF | kubectl --kubeconfig $KUBECONFIG1 apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: service-reflector
data:
  source-api: http://$(kubectl --kubeconfig $KUBECONFIG2 get service service-reflector -o jsonpath='{.spec.clusterIP}'):8080
EOF
# Restart the reflector.
kubectl --kubeconfig $KUBECONFIG1 delete pod --selector app.kubernetes.io/name=service-reflector
```
