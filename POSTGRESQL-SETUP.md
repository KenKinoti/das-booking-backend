# AGO CRM - PostgreSQL Database Setup

## Prerequisites

1. **PostgreSQL Installed**
   - Download from: https://www.postgresql.org/download/
   - Make sure it's running on port 5432
   - Default user: `postgres`, password: `admin`

2. **Go Environment**
   - Go 1.24+ installed
   - All dependencies downloaded (`go mod download`)

## Quick Setup Steps

### Option 1: Automatic Setup (Recommended)

1. **Run Database Setup**
   ```bash
   cd D:\DASYIN\gofiber-das-crm-backend
   setup-database.bat
   ```

2. **Start the Application**
   ```bash
   cd D:\DASYIN
   START-APP.bat
   # Choose option 2 for PostgreSQL
   ```

### Option 2: Manual Setup

1. **Create Database Manually**
   ```sql
   -- Connect to PostgreSQL as postgres user
   psql -U postgres
   
   -- Create database
   CREATE DATABASE das_booking_db;
   
   -- Grant privileges
   GRANT ALL PRIVILEGES ON DATABASE das_booking_db TO postgres;
   
   -- Exit psql
   \q
   ```

2. **Start Backend Server**
   ```bash
   cd D:\DASYIN\gofiber-das-crm-backend
   run-postgres.bat
   ```

## Database Configuration

The backend connects using these settings (from .env):

```env
DATABASE_URL=postgres://postgres:admin@localhost:5432/das_booking_db?sslmode=disable
DB_HOST=localhost
DB_PORT=5432
DB_USER=postgres
DB_PASSWORD=admin
DB_NAME=das_booking_db
```

## Database Schema

The Go backend automatically creates these tables:

- **users** - System users (admins, staff)
- **organizations** - Business organizations
- **customers** - End customers
- **bookings** - Appointment bookings
- **services** - Available services
- **staff** - Staff members

## Sample Data

The backend automatically seeds the database with:

✅ **Organizations**: AutoCare Plus, Beauty Haven Spa, Quick Fix Garage
✅ **Customers**: John Smith, Sarah Jones, Mike Brown
✅ **Services**: Oil Change, Brake Service, Tire Rotation, Hair Cut, Facial
✅ **Staff**: Mike Johnson, Tom Wilson, Lisa Davis, Emma Garcia
✅ **Users**: Super Admin, Garage Admin, Salon Manager
✅ **Bookings**: Sample appointment data

## API Endpoints

Once running, these endpoints are available:

- `GET /api/health` - Health check
- `POST /api/auth/login` - User authentication
- `GET /api/organizations` - List organizations
- `GET /api/bookings` - List bookings (with customer data)
- `GET /api/customers` - List customers
- `GET /api/services` - List services
- `GET /api/staff` - List staff members

## Testing the Connection

1. **Use the API Tester**
   - Open `test-api.html` in your browser
   - Click "Check Health" - should show PostgreSQL status
   - Test other endpoints

2. **Check Database Directly**
   ```sql
   psql -U postgres -d das_booking_db
   \dt  -- List all tables
   SELECT * FROM organizations;
   SELECT * FROM customers;
   ```

## Troubleshooting

### Connection Issues
- ✅ PostgreSQL service is running
- ✅ Port 5432 is not blocked by firewall
- ✅ Username/password is correct
- ✅ Database `das_booking_db` exists

### Permission Issues
```sql
-- Grant full access to postgres user
GRANT ALL PRIVILEGES ON DATABASE das_booking_db TO postgres;
GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA public TO postgres;
```

### Reset Database
```sql
DROP DATABASE IF EXISTS das_booking_db;
CREATE DATABASE das_booking_db;
-- Then restart the Go server
```

## Success Indicators

When everything works correctly, you should see:

```
🔗 Connecting to PostgreSQL...
   Database: das_booking_db
   Host: localhost:5432
✅ Successfully connected to PostgreSQL!
🔄 Running database migrations...
✅ Database migrations completed!
🌱 Seeding database with initial data...
✅ Database seeding completed!
🚀 AGO CRM PostgreSQL Server starting...
📍 Server: http://localhost:8080
✅ Server is ready to accept connections
💾 Using PostgreSQL database: das_booking_db
```

## Next Steps

1. Backend connects to PostgreSQL ✅
2. Frontend loads data from real database ✅
3. All CRUD operations work with PostgreSQL ✅
4. Data persists between server restarts ✅

Your AGO CRM system is now running with a full PostgreSQL database! 🎉