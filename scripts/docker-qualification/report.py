#!/usr/bin/env python3
"""Require actual test completion and evidence for every qualification stage."""
import json
from pathlib import Path
import sys

CHECKS = ('concurrent_jobs', 'controller_sigkill_replay', 'guest_relay_sigkill',
          'worker_endpoint_sigkill', 'docker_daemon_sigkill', 'cpu_memory_pressure',
          'private_disk_enospc', 'prune_isolation', 'successful_pair_cleanup',
          'primary_vm_loss', 'engine_vm_loss', 'fresh_generation_after_faults')


def summarize(out, exit_code):
    def read(name, default):
        p = out / name
        return json.loads(p.read_text()) if p.exists() else default
    events = [json.loads(line) for line in (out / 'tests.jsonl').read_text().splitlines()] if (out / 'tests.jsonl').exists() else []
    evidence = read('qualification.json', {})
    passed = any(e.get('Action') == 'pass' and e.get('Package') == 'github.com/colony-2/pulse-tart'
                 and e.get('Test') == 'TestLiveDockerQualification' for e in events)
    passed = passed and not any(e.get('Action') == 'fail' for e in events)
    checks = {key: evidence.get('checks', {}).get(key) is True for key in CHECKS}
    identities = evidence.get('identities', [])
    distinct = len(identities) == 6 and all(len({i.get(key) for i in identities}) == 6
                 and all(i.get(key) for i in identities)
                 for key in ('primary_uid', 'engine_id', 'client_key_sha256', 'host_key_sha256'))
    macs = len(identities) == 6 and all(len({i.get('mac') for i in identities[start:start+2]}) == 2
                and all(i.get('mac') for i in identities[start:start+2]) for start in (0, 2, 4))
    jobs = [json.loads(p.read_text()) for p in out.glob('jobdb-*.json')]
    jobs_ok = len(jobs) == 4 and all(j.get('status') == 'COMPLETED' and j.get('output_ok') is True
                   and j.get('claims') == 1 and j.get('renewals', 0) >= 2 and j.get('secret_calls') == 1 for j in jobs)
    peak = evidence.get('peak_running_macos', 0)
    success = exit_code == 0 and passed and all(checks.values()) and distinct and macs and jobs_ok and 0 < peak <= 2
    return {'scope': 'native-production-docker-qualification', 'result': 'passed' if success else 'incomplete',
            'checks': checks, 'distinct_allocation_identities': distinct, 'distinct_concurrent_macs': macs, 'completed_jobdb_jobs': len(jobs),
            'jobdb_valid': jobs_ok, 'peak_observed_running_macos': peak,
            'limits': ['bounded container CPU/memory and 8 GiB engine filesystem pressure',
                       'process and VM loss; physical host power loss is not injected',
                       'one native macOS worker; Orchard fault behavior is not certified by this run']}


if __name__ == '__main__':
    out = Path(sys.argv[1])
    try:
        result = summarize(out, int(sys.argv[2]))
    except (ValueError, TypeError, KeyError, OSError) as error:
        result = {'scope': 'native-production-docker-qualification', 'result': 'incomplete',
                  'error': 'Missing or invalid qualification evidence: ' + type(error).__name__}
    (out / 'report.json').write_text(json.dumps(result, indent=2) + '\n')
    print('Result:', result['result'])
    sys.exit(0 if result['result'] == 'passed' else 2)
