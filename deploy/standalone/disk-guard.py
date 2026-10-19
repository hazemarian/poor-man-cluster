#!/usr/bin/env python3
"""Pause only Nextrum collection when its data filesystems have <10GiB free."""
import os, shutil, subprocess
from pathlib import Path
os.chdir(Path(__file__).resolve().parent)
# Named Docker volumes may live on a different filesystem from this directory.
# A short-lived helper reads df for the actual backend volumes, without network,
# host writes, capabilities or access to the Docker socket.
report=subprocess.check_output([
    'docker','run','--rm','--network','none','--read-only','--cap-drop','ALL',
    '--memory','32m','--cpus','0.1','--log-driver','none',
    '--label','com.docker.compose.project=nextrum-monitoring',
    '--label','com.docker.compose.service=disk-guard',
    '-v','nextrum-monitoring_parseable-data:/check-data:ro',
    '-v','nextrum-monitoring_parseable-staging:/check-staging:ro',
    'alpine:3.20','df','-Pk','/check-data','/check-staging'],text=True,timeout=30)
free=[shutil.disk_usage('.').free]
free.extend(int(line.split()[3])*1024 for line in report.splitlines()[1:] if line.strip())
if min(free)<10*1024**3:
    running=subprocess.check_output(['docker','compose','ps','--status','running','-q','collector'],text=True,timeout=15).strip()
    if running:
        subprocess.run(['docker','compose','stop','collector'],check=True,timeout=30)
        print('Nextrum collector paused: data filesystem below10GiB; inspect before manual restart',flush=True)
