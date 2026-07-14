package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(func(app core.App) error {
		systems, err := app.FindCollectionByNameOrId("systems")
		if err != nil {
			return err
		}
		for _, field := range []core.Field{
			&core.TextField{Name: "power_network_id", Max: 64},
			&core.TextField{Name: "wol_interface", Max: 64},
			&core.TextField{Name: "wol_mac", Max: 32},
			&core.TextField{Name: "wol_broadcast", Max: 64},
			&core.NumberField{Name: "wol_port", OnlyInt: true},
			&core.BoolField{Name: "wol_enabled"},
			&core.BoolField{Name: "power_management_enabled"},
		} {
			if systems.Fields.GetByName(field.GetName()) == nil {
				systems.Fields.Add(field)
			}
		}
		if err := app.Save(systems); err != nil {
			return err
		}

		if _, err := app.FindCollectionByNameOrId("power_networks"); err != nil {
			collection := core.NewBaseCollection("power_networks")
			collection.Fields.Add(&core.TextField{Name: "name", Required: true, Max: 128})
			collection.Fields.Add(&core.TextField{Name: "interface", Max: 64})
			collection.Fields.Add(&core.TextField{Name: "source", Required: true, Max: 32})
			collection.Fields.Add(&core.TextField{Name: "prefix", Required: true, Max: 64})
			collection.Fields.Add(&core.TextField{Name: "broadcast", Required: true, Max: 64})
			collection.Fields.Add(&core.BoolField{Name: "enabled"})
			collection.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
			collection.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
			if err := app.Save(collection); err != nil {
				return err
			}
		}
		if _, err := app.FindCollectionByNameOrId("power_operations"); err != nil {
			collection := core.NewBaseCollection("power_operations")
			collection.Fields.Add(&core.RelationField{Name: "system", CollectionId: systems.Id, MaxSelect: 1, CascadeDelete: true})
			collection.Fields.Add(&core.TextField{Name: "operation", Required: true, Max: 32})
			collection.Fields.Add(&core.TextField{Name: "state", Required: true, Max: 32})
			collection.Fields.Add(&core.TextField{Name: "request_id", Required: true, Max: 64})
			collection.Fields.Add(&core.TextField{Name: "error_code", Max: 64})
			collection.Fields.Add(&core.TextField{Name: "actor", Max: 64})
			collection.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
			if err := app.Save(collection); err != nil {
				return err
			}
		}
		return nil
	}, func(app core.App) error {
		for _, name := range []string{"power_operations", "power_networks"} {
			if c, err := app.FindCollectionByNameOrId(name); err == nil {
				if err := app.Delete(c); err != nil {
					return err
				}
			}
		}
		systems, err := app.FindCollectionByNameOrId("systems")
		if err != nil {
			return err
		}
		for _, name := range []string{"power_network_id", "wol_interface", "wol_mac", "wol_broadcast", "wol_port", "wol_enabled", "power_management_enabled"} {
			systems.Fields.RemoveByName(name)
		}
		return app.Save(systems)
	})
}
