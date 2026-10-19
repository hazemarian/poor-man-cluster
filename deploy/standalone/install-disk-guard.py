#!/usr/bin/env python3
"""Install/remove only this deployment's user cron entry; preserve other jobs."""
import subprocess,sys,shlex
from pathlib import Path
marker='# nextrum-monitoring-disk-guard'
r=subprocess.run(['crontab','-l'],capture_output=True,text=True)
if r.returncode not in (0,1): raise SystemExit('Cannot read user crontab')
lines=[line for line in r.stdout.splitlines() if marker not in line]
if '--remove' not in sys.argv:
    root=shlex.quote(str(Path(__file__).resolve().parent))
    lines.append('*/5 * * * * cd '+root+' && /usr/bin/python3 disk-guard.py >> disk-guard.log 2>&1 '+marker)
subprocess.run(['crontab','-'],input='\n'.join(lines)+'\n',text=True,check=True)
print('Nextrum disk guard '+('removed' if '--remove' in sys.argv else 'installed'))
