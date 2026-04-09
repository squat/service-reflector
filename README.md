<p align="center"><img src="./service-reflector.svg" width="200"></p>

# Service-Reflector

Service-Reflector mirrors Kubernetes Services so that Pods running in one cluster can natively access Services from another cluster.

[![Build Status](https://github.com/squat/service-reflector/workflows/CI/badge.svg)](https://github.com/squat/service-reflector/actions?query=workflow%3ACI)
[![Go Report Card](https://goreportcard.com/badge/github.com/squat/service-reflector)](https://goreportcard.com/report/github.com/squat/service-reflector)
[![Built with Nix](https://img.shields.io/static/v1?logo=nixos&logoColor=white&label=&message=Built%20with%20Nix&color=41439a)](https://builtwithnix.org)

## Overview

A key use for multi-cluster Kubernetes is to allow Pods and Services in one cluster to access those running in another cluster.
However, a Pod in cluster A has no Kubernetes-native way of discovering a Service in cluster B or determining its endpoints; instead it must be pointed explicitly at the Service's ClusterIP.
One solution to this issue is for an administrator to create a Service in cluster A to mirror the one in cluster B with Endpoints that point at the Service’s Pods.
Service-Reflector automates the mirroring of selected Services from one cluster to another to simplify running multi-cluster Kubernetes.

## How it works

Service-Reflector comprises two components: a Reflector and an Emitter.
Both components are run by default, though either can be run independently or disabled, via the `--reflector` and `--emitter` flags.

### Reflector

The Reflector is a set of controllers that watches source APIs for Services to reflect by finding [ServiceExport](https://github.com/kubernetes-sigs/mcs-api/blob/master/config/crd/multicluster.x-k8s.io_serviceexports.yaml) resources and creates mirror Services and EndpointSlices in the host cluster.
One Reflector can watch many sources for Services, allowing a cluster to access Services from many different clusters.
The source APIs can be any Kubernetes-style API that allows listing and watching ServiceExports and EndpointSlices, for example:
* a Kubernetes apiserver, specified with the `--reflector.source-kubeconfig` flag; or
* an Emitter, specified with the `--reflector.source-api` flag.

### Emitter

In order allow a Reflector in cluster A to find Services in cluster B, we must point the Reflector at a source API, namely cluster B's Kubernetes apiserver.
Doing so requires configuring the Reflector with a Kubeconfig or ServiceAccount token from cluster B.
To simplify deployment, Service-Reflector provides an Emitter, a Kubernetes-style apiserver that implements only the list and watch verbs for ServiceExports and EndpointSlices.
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

## Usage

[embedmd]:# (help.txt)
```txt
Usage of service-reflector:
      --cluster-id string                              Name of this cluster (required).
      --emitter                                        Run the local Emitter API server. (default true)
      --emitter.bind-address ip                        The IP address on which to listen for the --secure-port port. The associated interface(s) must be reachable by the rest of the cluster, and by CLI/web clients. If blank or an unspecified address (0.0.0.0 or ::), all interfaces and IP address families will be used. (default 0.0.0.0)
      --emitter.cert-dir string                        The directory where the TLS certs are located. If --tls-cert-file and --tls-private-key-file are provided, this flag will be ignored. (default "emitter.local.config/certificates")
      --emitter.disable-http2-serving                  If true, HTTP2 serving will be disabled [default=false]
      --emitter.http2-max-streams-per-connection int   The limit that the server gives to clients for the maximum number of streams in an HTTP/2 connection. Zero means to use golang's default.
      --emitter.permit-address-sharing                 If true, SO_REUSEADDR will be used when binding the port. This allows binding to wildcard IPs like 0.0.0.0 and specific IPs in parallel, and it avoids waiting for the kernel to release sockets in TIME_WAIT state. [default=false]
      --emitter.permit-port-sharing                    If true, SO_REUSEPORT will be used when binding the port, which allows more than one instance to bind on the same address and port. [default=false]
      --emitter.secure-port int                        The port on which to serve HTTPS with authentication and authorization. If 0, don't serve HTTPS at all. (default 6443)
      --emitter.tls-cert-file string                   File containing the default x509 Certificate for HTTPS. (CA cert, if any, concatenated after server cert). If HTTPS serving is enabled, and --tls-cert-file and --tls-private-key-file are not provided, a self-signed certificate and key are generated for the public address and saved to the directory specified by --cert-dir.
      --emitter.tls-cipher-suites strings              Comma-separated list of cipher suites for the server. If omitted, the default Go cipher suites will be used. 
                                                       Preferred values: TLS_AES_128_GCM_SHA256, TLS_AES_256_GCM_SHA384, TLS_CHACHA20_POLY1305_SHA256, TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA, TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA, TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384, TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305, TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256, TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA, TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256, TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA, TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384, TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305, TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256. 
                                                       Insecure values: TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256, TLS_ECDHE_ECDSA_WITH_RC4_128_SHA, TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA, TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256, TLS_ECDHE_RSA_WITH_RC4_128_SHA, TLS_RSA_WITH_3DES_EDE_CBC_SHA, TLS_RSA_WITH_AES_128_CBC_SHA, TLS_RSA_WITH_AES_128_CBC_SHA256, TLS_RSA_WITH_AES_128_GCM_SHA256, TLS_RSA_WITH_AES_256_CBC_SHA, TLS_RSA_WITH_AES_256_GCM_SHA384, TLS_RSA_WITH_RC4_128_SHA.
      --emitter.tls-min-version string                 Minimum TLS version supported. Possible values: VersionTLS10, VersionTLS11, VersionTLS12, VersionTLS13
      --emitter.tls-private-key-file string            File containing the default x509 private key matching --tls-cert-file.
      --emitter.tls-sni-cert-key namedCertKey          A pair of x509 certificate and private key file paths, optionally suffixed with a list of domain patterns which are fully qualified domain names, possibly with prefixed wildcard segments. The domain patterns also allow IP addresses, but IPs should only be used if the apiserver has visibility to the IP address requested by a client. If no domain patterns are provided, the names of the certificate are extracted. Non-wildcard matches trump over wildcard matches, explicit domain patterns trump over extracted names. For multiple key/certificate pairs, use the --tls-sni-cert-key multiple times. Examples: "example.crt,example.key" or "foo.crt,foo.key:*.foo.com,foo.com". (default [])
      --kubeconfig string                              Path to kubeconfig for the local cluster.
      --listen string                                  Address to listen for health and metrics. (default ":9090")
      --log-level string                               Log level to use. Possible values: debug, info, warn, error (default "info")
      --namespace string                               Namespace to watch (empty = all).
      --source-api url                                 URL of a remote Emitter to watch (repeatable).
      --source-kubeconfig stringArray                  Kubeconfig for a remote cluster (repeatable).
      --version                                        Print version and exit.
```
