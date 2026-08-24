# A .uf2 is written to the board as-is.
tinygo build -target=pico -o binary.uf2 ./project
curl -F firmware=@binary.uf2 http://192.168.1.100:8080/devices/2e8a:000a@3-2/flash

# An .elf works too: picohub converts it to a UF2 for the board's chip.
# Keep the ELF around if you also want to debug — a .uf2 carries no symbols.
tinygo build -target=pico -o binary.elf ./project
curl -F firmware=@binary.elf http://192.168.1.100:8080/devices/2e8a:000a@3-2/flash
