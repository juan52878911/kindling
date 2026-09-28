-- Esquema y datos por defecto del banco: 100000 filas en items. Es lo que se
-- aplica en los modos docker y template; el golden de kindling ya lo trae
-- horneado (mismo contenido, mismo recuento esperado: -expect 100000).
CREATE TABLE items (
    id      integer PRIMARY KEY,
    name    text NOT NULL,
    payload text NOT NULL
);
INSERT INTO items (id, name, payload)
SELECT g, 'item-' || g, md5(g::text)
FROM generate_series(1, 100000) AS g;
CREATE INDEX items_name_idx ON items (name);
