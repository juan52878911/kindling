create table items(id int primary key, payload text not null, created_at timestamptz not null default now());
insert into items(id, payload) select g, repeat(md5(g::text), 3) from generate_series(1, 400000) g;
analyze items;
