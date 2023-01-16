#!/usr/bin/env bash

# start the lima instance with the given image and get the kube config
limactl start --tty=false --name=default ./${LIMA_CONFIG}.yaml

# for cgroups v1, reconfigure grub and restart the instance
# we need to both call the reboot command and do a lima stop/start
# for the instance to be working (it rebinds things such as ssh to the host)
if [[ ${LIMA_CGROUPS} == "v1" ]]; then
  echo "Reconfiguring lima instance with cgroups v1"
  limactl shell default sudo sed -i 's/GRUB_CMDLINE_LINUX=""/GRUB_CMDLINE_LINUX="systemd.unified_cgroup_hierarchy=0"/' /etc/default/grub
  limactl shell default sudo update-grub
  limactl shell default sudo reboot
  echo "Waiting for instance to reboot, it might take a while"
  sleep 5
  limactl stop default
  limactl start default
fi
