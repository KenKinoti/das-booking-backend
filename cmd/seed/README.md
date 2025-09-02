# Database Seeder

This directory contains the database seeder for the NDIS CRM system. The seeder creates realistic test data that can be used for development, testing, and demonstrations.

## Quick Start

### Seed the database with test data:
```bash
# From the project root
make seed
# OR
./scripts/seed.sh
# OR
go run cmd/seed/main.go
```

### Remove all test data:
```bash
# From the project root
make seed-clean
# OR
./scripts/seed.sh clean
# OR
go run cmd/seed/main.go --clean
```

## What gets created

### Organizations (2)
- **Sunshine Care Services** (Sydney-based)
- **Melbourne Support Network** (Melbourne-based)

### Users (16 per organization + 1 super admin)
- **1 Super Admin**: `superadmin@system.com` (system-wide access)
- **2 Admins** per organization (full org management)
- **2 Managers** per organization (staff & scheduling)
- **2 Support Coordinators** per organization (participant care)
- **2 Care Workers** per organization (shift execution)

All users have the password: `Test123!@#`

### Participants (15-20 per organization)
- Realistic names, addresses, and contact information
- NDIS numbers and funding information
- Medical conditions, medications, and allergies
- 1-2 emergency contacts each
- Mix of active and inactive participants (90% active)

### Shifts (200+ per organization)
- **Past month**: Mix of completed, cancelled, and in-progress shifts
- **Upcoming week**: Scheduled shifts
- Realistic timeframes (2-8 hour shifts between 6 AM - 6 PM)
- Various service types: Personal Care, Community Access, Transport, etc.
- Associated with random participants and care workers

### Care Plans (1 per participant)
- NDIS-aligned support plans
- Goals and review dates
- Mix of active, pending, and under-review status

## Features

### Realistic Data
- Australian addresses and phone numbers
- Proper NDIS number format
- Realistic medical conditions and funding amounts
- Age-appropriate participants (18-80 years old)

### Easy Cleanup
- All test data is marked for easy identification
- Single-command cleanup removes only test data
- Preserves any production data you may have added

### Safe Operation
- Won't duplicate data if run multiple times
- Includes validation and error handling
- Respects foreign key constraints

## Technical Details

### Dependencies
- Uses GORM for database operations
- Generates UUIDs for all primary keys
- Bcrypt for password hashing
- Follows the same model structure as the main application

### Database Support
- Currently configured for MySQL
- Can be easily adapted for PostgreSQL or other GORM-supported databases

### Performance
- Uses batch operations where possible
- Typically completes in 10-30 seconds depending on database performance

## Customization

You can modify the seeder by editing:
- `firstNames`, `lastNames`: Change the pool of names used
- `conditions`, `medications`: Update medical information options
- `suburbs`: Modify location data
- Number of entities created in each `Seed*` function

## Safety Notes

⚠️ **Important**: 
- Always use `--clean` or `seed-clean` on production databases to avoid data loss
- The seeder is designed to be safe but always backup production data first
- Test accounts use obvious test emails and are marked in the notes field

## Troubleshooting

### "Access denied" errors
- Check your database credentials in the config
- Ensure the database user has CREATE, INSERT, DELETE permissions

### "Table doesn't exist" errors
- Run the main application first to create tables via auto-migration
- Or create a separate migration command

### "Duplicate entry" errors  
- The seeder includes logic to handle duplicates
- If issues persist, run `make seed-clean` first

### Performance issues
- Large datasets may take time to create
- Consider reducing the number of shifts or participants for faster seeding
- Ensure your database has adequate resources