-- The DOS quotation template has separate Delivery, Payment terms and
-- Warranty fields; "terms" stays for anything else.
ALTER TABLE quotes ADD COLUMN delivery_terms text;
ALTER TABLE quotes ADD COLUMN payment_terms  text;
ALTER TABLE quotes ADD COLUMN warranty_terms text;
