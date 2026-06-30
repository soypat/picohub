
rsync -av --delete \
  --exclude='picohub-logs/' \
  --exclude='picohub.db' \
  --exclude='picohub' \
  --exclude='.git/' \
  --exclude='*.uf2' \
  --exclude='local/' \
  /home/pato/Documents/src/tg/picohub/ \
  pato@192.168.1.100:~/picohub/

# Link the unit from the synced copy, reload, and restart.
ssh pato@192.168.1.100 'set -e
  sudo ln -sf ~/picohub/picohub.service /etc/systemd/system/picohub.service
  sudo systemctl daemon-reload
  sudo systemctl restart picohub'