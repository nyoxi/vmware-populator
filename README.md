# vmware-populator
 
[Volume Populator](https://kubernetes.io/blog/2022/05/16/volume-populators-beta/) that uses disks from VMware as a source.
Currently only tested on Openshift

## Installation

First deploy the Volume Datasrouce Validator:

```shell
$ oc create -f https://raw.githubusercontent.com/kubernetes-csi/volume-data-source-validator/v1.0.1/client/config/crd/populator.storage.k8s.io_volumepopulators.yaml
$ oc create -f https://raw.githubusercontent.com/kubernetes-csi/volume-data-source-validator/v1.0.1/deploy/kubernetes/rbac-data-source-validator.yaml
$ oc create -f https://raw.githubusercontent.com/kubernetes-csi/volume-data-source-validator/v1.0.1/deploy/kubernetes/setup-data-source-validator.yaml
```

For Openshift and Kuberenetes < 1.24 enable the AnyVolumeDataSource feature gate. Edit the featuregate CR:

```shell
$ oc edit featuregate cluster
```

Add the following into the CR to enable the feature gate:

```yaml
    ...
    spec:
      ...
      customNoUpgrade:
        enabled:
        - AnyVolumeDataSource
      featureSet: CustomNoUpgrade
```

Deploy the volume populator:

```shell
$ oc create -f crd.yaml
$ oc create -f deploy.yaml
```
