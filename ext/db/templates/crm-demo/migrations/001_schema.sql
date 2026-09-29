-- crm-demo: esquema. Lo ejecuta el rol de la aplicación (es dueño de todo).

CREATE TABLE products (
  id          integer PRIMARY KEY,
  sku         text NOT NULL UNIQUE,
  name        text NOT NULL,
  category    text NOT NULL CHECK (category IN ('software', 'support', 'training', 'hardware')),
  unit_price  numeric(10,2) NOT NULL CHECK (unit_price > 0)
);

CREATE TABLE customers (
  id          integer PRIMARY KEY,
  name        text NOT NULL,
  industry    text NOT NULL,
  country     text NOT NULL,
  tier        text NOT NULL CHECK (tier IN ('free', 'pro', 'enterprise')),
  created_at  timestamptz NOT NULL
);

CREATE TABLE contacts (
  id           integer PRIMARY KEY,
  customer_id  integer NOT NULL REFERENCES customers (id),
  first_name   text NOT NULL,
  last_name    text NOT NULL,
  email        text NOT NULL UNIQUE,
  phone        text,
  job_title    text NOT NULL,
  created_at   timestamptz NOT NULL
);

CREATE TABLE deals (
  id           integer PRIMARY KEY,
  customer_id  integer NOT NULL REFERENCES customers (id),
  contact_id   integer NOT NULL REFERENCES contacts (id),
  product_id   integer NOT NULL REFERENCES products (id),
  title        text NOT NULL,
  stage        text NOT NULL CHECK (stage IN ('lead', 'qualified', 'proposal', 'negotiation', 'won', 'lost')),
  amount       numeric(12,2) NOT NULL CHECK (amount >= 0),
  created_at   timestamptz NOT NULL,
  closed_at    timestamptz,
  CHECK ((stage IN ('won', 'lost')) = (closed_at IS NOT NULL)),
  CHECK (closed_at IS NULL OR closed_at >= created_at)
);

CREATE TABLE activities (
  id           integer PRIMARY KEY,
  deal_id      integer NOT NULL REFERENCES deals (id),
  contact_id   integer NOT NULL REFERENCES contacts (id),
  kind         text NOT NULL CHECK (kind IN ('call', 'email', 'meeting', 'note')),
  occurred_at  timestamptz NOT NULL,
  summary      text NOT NULL
);

CREATE TABLE invoices (
  id           integer PRIMARY KEY,
  customer_id  integer NOT NULL REFERENCES customers (id),
  deal_id      integer NOT NULL REFERENCES deals (id),
  number       text NOT NULL UNIQUE,
  issued_on    date NOT NULL,
  due_on       date NOT NULL,
  amount       numeric(12,2) NOT NULL CHECK (amount > 0),
  status       text NOT NULL CHECK (status IN ('draft', 'sent', 'paid', 'overdue', 'void')),
  CHECK (due_on >= issued_on)
);

CREATE INDEX contacts_customer_idx   ON contacts (customer_id);
CREATE INDEX deals_customer_idx      ON deals (customer_id);
CREATE INDEX deals_contact_idx       ON deals (contact_id);
CREATE INDEX deals_stage_idx         ON deals (stage);
CREATE INDEX activities_deal_idx     ON activities (deal_id);
CREATE INDEX activities_contact_idx  ON activities (contact_id);
CREATE INDEX invoices_customer_idx   ON invoices (customer_id);
CREATE INDEX invoices_deal_idx       ON invoices (deal_id);
CREATE INDEX invoices_status_idx     ON invoices (status);
