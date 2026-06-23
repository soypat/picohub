
rsync -av --delete \
  --exclude='picohub-logs/' \
  --exclude='picohub.db' \
  --exclude='picohub' \
  --exclude='.git/' \
  /home/pato/Documents/src/tg/picohub/ \
  pato@192.168.1.100:~/picohub/