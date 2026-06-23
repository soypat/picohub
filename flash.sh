tinygo build -target=pico -o binary.uf2 ./project
curl -F firmware=@binary.uf2 http://192.168.1.100:8080/devices/2e8a:000a@3-2/flash
