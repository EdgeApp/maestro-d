#!/usr/bin/env python3
"""Fail closed on missing Go instrumentation; report changed covered block lines."""
import argparse
import hashlib
import json
import pathlib
import re
import subprocess


def git(*args):
    return subprocess.check_output(['git', *args], text=True).strip()


def digest():
    paths = set(git('ls-files', '-co', '--exclude-standard').splitlines())
    value = hashlib.sha256()
    for name in sorted(paths):
        path = pathlib.Path(name)
        if path.is_file() and path.suffix in ('.go', '.py', '.sh'):
            value.update(name.encode())
            value.update(path.read_bytes())
    return value.hexdigest()


def changed(diff):
    result = {}
    name = None
    line = 0
    for text in diff.splitlines():
        if text.startswith('+++ b/'):
            name = text[6:]
        elif text.startswith('@@'):
            line = int(re.search(r'\+(\d+)', text).group(1))
        elif text.startswith('+') and name is not None:
            result.setdefault(name, set()).add(line)
            line += 1
        elif not text.startswith(('-', '\\')):
            line += 1
    return result


def counters(profile, module):
    result = {}
    for row in profile.splitlines()[1:]:
        match = re.fullmatch(r'(.+):(\d+)\.\d+,(\d+)\.\d+ (\d+) (\d+)', row)
        if not match:
            raise ValueError('Invalid Go coverage row')
        name, start, end, statements, count = match.groups()
        if name.startswith(module + '/'):
            name = name[len(module) + 1:]
        lines = result.setdefault(name, {})
        if int(statements) == 0:
            continue
        for line in range(int(start), int(end) + 1):
            # A partly uncovered line is still uncovered, even when another
            # branch on the same line ran. Never inflate branch coverage.
            lines[line] = min(lines.get(line, int(count)), int(count))
    return result


def report(edits, measured, minimum):
    files = {}
    missing = []
    covered = total = 0
    for name, lines in sorted(edits.items()):
        if not name.endswith('.go') or name.endswith('_test.go'):
            continue
        if name not in measured:
            missing.append(name)
            continue
        executable = lines & measured[name].keys()
        hits = sorted(line for line in executable if measured[name][line] > 0)
        misses = sorted(executable - set(hits))
        size = len(executable)
        percent = 100 * len(hits) / size if size else 100
        files[name] = dict(covered=len(hits), total=size, percent=percent, uncovered=misses)
        covered += len(hits)
        total += size
    percent = 100 * covered / total if total else 0
    return dict(files=files, missing=missing, covered=covered, total=total, percent=percent,
                passed=not missing and total > 0 and percent >= minimum and
                all(f['percent'] >= minimum for f in files.values()))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--digest', action='store_true')
    parser.add_argument('--base')
    parser.add_argument('--profile')
    parser.add_argument('--minimum', type=float, default=95)
    args = parser.parse_args()
    if args.digest:
        print(digest())
        return
    if not args.base or not args.profile:
        parser.error('--base and --profile are required')
    edits = changed(git('diff', '--unified=0', args.base, '--', '*.go'))
    for name in git('ls-files', '--others', '--exclude-standard').splitlines():
        if name.endswith('.go'):
            edits[name] = set(range(1, len(pathlib.Path(name).read_text().splitlines()) + 1))
    module = re.search(r'^module\s+(\S+)', pathlib.Path('go.mod').read_text(), re.M).group(1)
    value = report(edits, counters(pathlib.Path(args.profile).read_text(), module), args.minimum)
    pathlib.Path(args.profile + '.json').write_text(json.dumps(value, indent=2) + '\n')
    for name, row in value['files'].items():
        print(f"{name}: {row['percent']:.2f}% ({row['covered']}/{row['total']}), uncovered {row['uncovered']}")
    print(f"Combined changed blocks: {value['percent']:.2f}% ({value['covered']}/{value['total']}); missing {value['missing']}")
    if not value['passed']:
        raise SystemExit(1)


if __name__ == '__main__':
    main()
