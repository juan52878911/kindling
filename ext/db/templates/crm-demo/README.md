# crm-demo

A small CRM with about 50,000 synthetic rows, the same every time it is built.

Tables: `products` (60), `customers` (2,000), `contacts` (5,000), `deals` (8,000,
stages lead / qualified / proposal / negotiation / won / lost), `activities`
(30,000) and `invoices` (only for won deals, about 4,000).

All data is made up: names come from syllables, e-mails use the reserved
`example.com` domain, phone numbers use the fictional `555-01xx` range. Dates hang from fixed
days (no now()), so two builds give identical data.

    kling db golden build -template crm-demo crm
    kling db up crm
    kling db role <copy> -ro
