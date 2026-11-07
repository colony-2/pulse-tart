#!/usr/bin/env python3
"""Shared naming convention for prepared Tart sources (no release metadata)."""
import hashlib
import json
import re
import sys


def token(value):
    if len(value) <= 24 and re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._-]*', value):
        return value
    return 'id-' + hashlib.sha256(value.encode()).hexdigest()[:20]


def names(owner, base, group, cpu, memory, count):
    prefix = f'pulse-{token(owner)}-{token(base)}'
    return [prefix+'-base'] + [f'{prefix}-{token(group)}-{cpu}cpu-{memory}m-slot-{i:04d}' for i in range(1, count+1)]


def aliases(canonical, inventory):
    # Orchard's native naming is orchard-<API name>-<UUID>-<restart count>.
    pattern = re.compile(r'^orchard-'+re.escape(canonical)+r'-[0-9a-fA-F]{8}(?:-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}-[0-9]+$')
    return [canonical] + sorted(item.get('Name', item.get('name', '')) for item in inventory
                                if pattern.fullmatch(item.get('Name', item.get('name', ''))))


if __name__ == '__main__':
    if sys.argv[1] == 'aliases':
        print('\n'.join(aliases(sys.argv[2], json.load(sys.stdin))))
    else:
        print('\n'.join(names(*sys.argv[1:4], *(int(v) for v in sys.argv[4:7]))))
