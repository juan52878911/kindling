-- crm-demo: datos sintéticos y deterministas (~50k filas). Nada aleatorio ni
-- dependiente de la hora: cada valor sale de un hash md5 del par (fila, columna) y las fechas
-- cuelgan de un día fijo, así dos construcciones dan exactamente lo mismo.
-- Nada es real: los nombres salen de sílabas, los correos son de example.com y
-- los teléfonos de la gama ficticia 555-01xx.

-- Entero determinista en [0, m) para (a, b).
CREATE FUNCTION pg_temp.h(a integer, b integer, m integer) RETURNS integer
LANGUAGE sql IMMUTABLE AS $$
  SELECT (('x' || substr(md5(a::text || ':' || b::text), 1, 7))::bit(28)::integer) % m
$$;

-- Nombre de 2 o 3 sílabas, con la primera en mayúscula.
CREATE FUNCTION pg_temp.nm(a integer, b integer) RETURNS text
LANGUAGE sql IMMUTABLE AS $$
  SELECT initcap(
    s[1 + pg_temp.h(a, b * 10 + 1, 16)] || s[1 + pg_temp.h(a, b * 10 + 2, 16)] ||
    CASE WHEN pg_temp.h(a, b * 10 + 3, 3) = 0 THEN s[1 + pg_temp.h(a, b * 10 + 4, 16)] ELSE '' END)
  FROM (SELECT ARRAY['ka','vor','tel','min','dra','sul','en','ba','lo','ri','zu','mar','fen','qui','sha','to'] AS s) x
$$;

-- Elemento h-ésimo de un array (a, b) -> arr[1 + h(a, b, n)].
CREATE FUNCTION pg_temp.pick(arr text[], a integer, b integer) RETURNS text
LANGUAGE sql IMMUTABLE AS $$
  SELECT arr[1 + pg_temp.h(a, b, array_length(arr, 1))]
$$;

INSERT INTO products (id, sku, name, category, unit_price)
SELECT g,
       'SKU-' || lpad(g::text, 4, '0'),
       pg_temp.pick(ARRAY['Core','Prime','Flex','Nova','Apex','Terra','Lumen','Vertex'], g, 1) || ' ' ||
       pg_temp.pick(ARRAY['Suite','Cloud','Module','Analytics','Gateway','Plan','Workshop','Sensor','Pack','License'], g, 2),
       (ARRAY['software', 'support', 'training', 'hardware'])[1 + g % 4],
       round((10 + pg_temp.h(g, 3, 99000) / 100.0)::numeric, 2)
FROM generate_series(1, 60) AS g;

INSERT INTO customers (id, name, industry, country, tier, created_at)
SELECT g,
       pg_temp.nm(g, 1) || ' ' || pg_temp.pick(ARRAY['Labs','Group','Systems','Partners','Holdings','Works'], g, 2),
       pg_temp.pick(ARRAY['retail','logistics','health','finance','education','energy','media','manufacturing'], g, 3),
       pg_temp.pick(ARRAY['ES','FR','DE','IT','PT','NL','GB','IE','MX','CO'], g, 4),
       CASE WHEN pg_temp.h(g, 5, 100) < 60 THEN 'free' WHEN pg_temp.h(g, 5, 100) < 90 THEN 'pro' ELSE 'enterprise' END,
       timestamptz '2023-01-01 00:00:00+00' + pg_temp.h(g, 6, 700) * interval '1 day' + pg_temp.h(g, 7, 86400) * interval '1 second'
FROM generate_series(1, 2000) AS g;

-- Cada cliente tiene 2 o 3 contactos: el contacto g es del cliente 1 + (g-1) % 2000.
INSERT INTO contacts (id, customer_id, first_name, last_name, email, phone, job_title, created_at)
SELECT g, c.id,
       f, l,
       lower(f || '.' || l || g) || '@example.com',
       CASE WHEN pg_temp.h(g, 13, 10) < 8 THEN '+1-555-01' || lpad(pg_temp.h(g, 14, 100)::text, 2, '0') END,
       pg_temp.pick(ARRAY['CEO','CTO','Head of Operations','Purchasing Manager','Data Analyst','Office Manager','Sales Director','Engineer'], g, 15),
       c.created_at + pg_temp.h(g, 16, 30 * 86400) * interval '1 second'
FROM generate_series(1, 5000) AS g
CROSS JOIN LATERAL (SELECT pg_temp.nm(g, 11) AS f, pg_temp.nm(g, 12) AS l) n
JOIN customers c ON c.id = 1 + (g - 1) % 2000;

-- Las oportunidades usan un contacto del propio cliente: cliente k, contactos
-- k, k+2000 y (si existe) k+4000.
INSERT INTO deals (id, customer_id, contact_id, product_id, title, stage, amount, created_at, closed_at)
SELECT d.g, d.cust, d.cust + 2000 * pg_temp.h(d.g, 21, CASE WHEN d.cust <= 1000 THEN 3 ELSE 2 END),
       p.id, p.name || ' for ' || cu.name, d.stage,
       round((p.unit_price * (1 + pg_temp.h(d.g, 24, 50)) * (80 + pg_temp.h(d.g, 25, 41)) / 100.0)::numeric, 2),
       d.created,
       CASE WHEN d.stage IN ('won', 'lost') THEN d.created + (1 + pg_temp.h(d.g, 28, 90)) * interval '1 day' END
FROM (
  SELECT g, 1 + pg_temp.h(g, 20, 2000) AS cust, 1 + pg_temp.h(g, 22, 60) AS prod,
         CASE WHEN r < 15 THEN 'lead' WHEN r < 30 THEN 'qualified' WHEN r < 42 THEN 'proposal'
              WHEN r < 52 THEN 'negotiation' WHEN r < 77 THEN 'won' ELSE 'lost' END AS stage,
         timestamptz '2024-01-01 00:00:00+00' + pg_temp.h(g, 26, 365) * interval '1 day' + pg_temp.h(g, 27, 86400) * interval '1 second' AS created
  FROM generate_series(1, 8000) AS g
  CROSS JOIN LATERAL (SELECT pg_temp.h(g, 23, 100) AS r) x
) d
JOIN products p ON p.id = d.prod
JOIN customers cu ON cu.id = d.cust;

INSERT INTO activities (id, deal_id, contact_id, kind, occurred_at, summary)
SELECT g, d.id, d.contact_id, k.kind,
       d.created_at + pg_temp.h(g, 31, 60 * 86400) * interval '1 second',
       CASE k.kind
         WHEN 'call'    THEN pg_temp.pick(ARRAY['Intro call','Follow-up call','Pricing questions','Renewal check-in'], g, 33)
         WHEN 'email'   THEN pg_temp.pick(ARRAY['Sent proposal','Sent case study','Replied to questions','Sent contract draft'], g, 33)
         WHEN 'meeting' THEN pg_temp.pick(ARRAY['Product demo','On-site visit','Kick-off meeting','Quarterly review'], g, 33)
         ELSE                pg_temp.pick(ARRAY['Budget approved','Waiting on legal','Champion changed jobs','Needs a discount'], g, 33)
       END
FROM generate_series(1, 30000) AS g
CROSS JOIN LATERAL (SELECT (ARRAY['call', 'email', 'meeting', 'note'])[1 + pg_temp.h(g, 32, 4)] AS kind) k
JOIN deals d ON d.id = 1 + pg_temp.h(g, 30, 8000);

-- Dos facturas por oportunidad ganada: 30 % de anticipo y el resto.
INSERT INTO invoices (id, customer_id, deal_id, number, issued_on, due_on, amount, status)
SELECT i, customer_id, deal_id, 'INV-' || lpad(i::text, 6, '0'), issued, issued + 30,
       round(amount * frac, 2),
       CASE WHEN r < 70 THEN 'paid' WHEN r < 85 THEN 'sent' WHEN r < 93 THEN 'overdue' WHEN r < 97 THEN 'draft' ELSE 'void' END
FROM (
  SELECT row_number() OVER (ORDER BY d.id, p.part)::integer AS i,
         d.customer_id, d.id AS deal_id, d.amount,
         (d.closed_at::date + (p.part - 1) * 30) AS issued,
         p.frac,
         pg_temp.h(d.id, p.part + 40, 100) AS r
  FROM deals d
  CROSS JOIN (VALUES (1, 0.30), (2, 0.70)) AS p (part, frac)
  WHERE d.stage = 'won'
) x;

ANALYZE products;
ANALYZE customers;
ANALYZE contacts;
ANALYZE deals;
ANALYZE activities;
ANALYZE invoices;
