import copy
import importlib.util
import pathlib
import unittest

spec = importlib.util.spec_from_file_location('acceptance', pathlib.Path(__file__).with_name('accept-openshift.py'))
acceptance = importlib.util.module_from_spec(spec)
spec.loader.exec_module(acceptance)
IMAGE = 'registry.invalid/gateway@sha256:' + 'a' * 64


class AdmissionValidation(unittest.TestCase):
    def setUp(self):
        self.deployment = {'metadata': {'name': 'smbfileserver', 'generation': 2}, 'spec': {'replicas': 1},
                           'status': {'observedGeneration': 2, 'updatedReplicas': 1, 'availableReplicas': 1}}
        self.pods = {'items': [{'metadata': {'name': 'example', 'annotations': {'openshift.io/scc': 'restricted-v2'}},
          'status': {'conditions': [{'type': 'Ready', 'status': 'True'}]},
          'spec': {'automountServiceAccountToken': False,
            'securityContext': {'runAsUser': 1001234567, 'runAsNonRoot': True, 'seccompProfile': {'type': 'RuntimeDefault'}},
            'containers': [{'name': 'gateway', 'image': IMAGE, 'securityContext': {'readOnlyRootFilesystem': True,
              'allowPrivilegeEscalation': False, 'capabilities': {'drop': ['ALL']}}}]}}]}
        self.route = {'spec': {'host': 'example.invalid', 'to': {'name': 'smbfileserver'}, 'port': {'targetPort': 'http'},
          'tls': {'termination': 'edge', 'insecureEdgeTerminationPolicy': 'Redirect'}},
          'status': {'ingress': [{'host': 'example.invalid', 'conditions': [{'type': 'Admitted', 'status': 'True'}]}]}}

    def test_admitted_snapshot(self):
        admitted, host = acceptance.validate_cluster(self.deployment, self.pods, self.route, IMAGE)
        self.assertEqual(admitted[0]['assigned_uid'], 1001234567)
        self.assertEqual(host, 'example.invalid')

    def test_rejects_weakened_security_and_unready_pods(self):
        mutations = [lambda p: p['metadata']['annotations'].update({'openshift.io/scc': 'anyuid'}),
                     lambda p: p['spec']['securityContext'].update({'runAsUser': 0}),
                     lambda p: p['spec']['containers'][0]['securityContext'].update({'readOnlyRootFilesystem': False}),
                     lambda p: p['spec']['containers'][0].update({'image': 'unexpected:latest'}),
                     lambda p: p['status'].update({'conditions': []})]
        for mutate in mutations:
            pods = copy.deepcopy(self.pods)
            mutate(pods['items'][0])
            with self.assertRaises(RuntimeError):
                acceptance.validate_cluster(self.deployment, pods, self.route, IMAGE)

    def test_rejects_unadmitted_route_and_stale_rollout(self):
        self.route['status']['ingress'] = []
        with self.assertRaises(RuntimeError):
            acceptance.validate_cluster(self.deployment, self.pods, self.route, IMAGE)
        self.setUp()
        self.deployment['status']['observedGeneration'] = 1
        with self.assertRaises(RuntimeError):
            acceptance.validate_cluster(self.deployment, self.pods, self.route, IMAGE)


if __name__ == '__main__':
    unittest.main()
