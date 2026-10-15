module mosagentcli

go 1.26

require (
	github.com/google/uuid v1.6.0
	github.com/puras/mos v0.0.1
)

require (
	github.com/puras/moproto v0.1.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/puras/mos v0.0.1 => ../..

replace github.com/puras/moproto v0.1.0 => ../../../moproto
