"""Keep public captured-module routes reachable without shadowing other services."""
from pathlib import Path
import re
import unittest


class CoreRoutes(unittest.TestCase):
    def test_captured_module_paths_belong_to_core(self):
        score = Path(__file__).resolve().parents[1].joinpath('score.yaml').read_text()
        routes = re.findall(r'^\s+path:\s+(/.*)$', score, re.MULTILINE)
        org_routes = [route for route in routes if 'orgs' in route]
        self.assertEqual(len(org_routes), 1, 'Review the Core route selector if its Score annotation changes')
        pattern = re.compile(org_routes[0])
        base = '/orgs/owned/projects/project/envs/development'
        for path in [base + '/modules/marker/drafts/draft-id/test-snapshots',
                     base + '/modules/marker/draft-test-snapshots/snapshot-id',
                     base + '/module-catalogue', base + '/pipeline-choices']:
            with self.subTest(path=path):
                self.assertIsNotNone(pattern.fullmatch(path))
        for path in [base + '/deployment-pipelines', base + '/runner-rollouts',
                     base + '/users', base + '/modules-elsewhere/marker',
                     '/orgs/owned/permissions', '/orgs/owned/module-test-deployments']:
            with self.subTest(path=path):
                self.assertIsNone(pattern.fullmatch(path))


if __name__ == '__main__':
    unittest.main()
