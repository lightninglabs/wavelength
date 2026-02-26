DROP INDEX IF EXISTS idx_oor_vtxo_bindings_session;
DROP INDEX IF EXISTS idx_oor_package_checkpoints_session;
DROP INDEX IF EXISTS idx_oor_packages_direction_updated;

DROP TABLE IF EXISTS owned_receive_scripts;
DROP TABLE IF EXISTS oor_recipient_cursors;
DROP TABLE IF EXISTS oor_vtxo_bindings;
DROP TABLE IF EXISTS oor_package_checkpoints;
DROP TABLE IF EXISTS oor_packages;
DROP TABLE IF EXISTS owned_receive_script_sources;
DROP TABLE IF EXISTS oor_vtxo_binding_link_kinds;
DROP TABLE IF EXISTS oor_package_directions;
