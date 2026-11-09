"""Apply the maintained counter patch only to its reviewed upstream commit."""
from pathlib import Path
import subprocess
import sys

PINNED_COMMIT = 'c5fe3670eff8ac030702959099505522aa7c0caa'  # v1.19.15

def main():
    source = Path(sys.argv[1]).resolve()
    commit = subprocess.check_output(['git', '-C', str(source), 'rev-parse', 'HEAD'], text=True).strip()
    if commit != PINNED_COMMIT:
        raise SystemExit('Refusing an unreviewed mihomo source version')
    patch = Path(__file__).resolve().parents[2] / 'scripts' / 'mihomo-meter.patch'
    subprocess.run(['git', '-C', str(source), 'apply', '--check', str(patch)], check=True)
    subprocess.run(['git', '-C', str(source), 'apply', str(patch)], check=True)

if __name__ == '__main__':
    main()
