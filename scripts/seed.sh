#!/bin/bash

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN}         NDIS CRM Database Seeder        ${NC}"
echo -e "${GREEN}========================================${NC}"
echo

# Check if we should clean data
if [[ "$1" == "clean" || "$1" == "--clean" ]]; then
    echo -e "${YELLOW}⚠ WARNING: This will remove all test data!${NC}"
    echo -n "Are you sure? (y/N): "
    read -r confirm
    
    if [[ $confirm == [yY] || $confirm == [yY][eE][sS] ]]; then
        echo -e "${GREEN}Cleaning test data...${NC}"
        go run cmd/seed/main.go --clean
    else
        echo -e "${YELLOW}Operation cancelled.${NC}"
        exit 0
    fi
else
    # Check if test data already exists
    echo -e "${GREEN}Checking for existing test data...${NC}"
    
    # Run the seeder
    echo -e "${GREEN}Starting database seeding process...${NC}"
    go run cmd/seed/main.go
    
    if [ $? -eq 0 ]; then
        echo
        echo -e "${GREEN}========================================${NC}"
        echo -e "${GREEN}         SEEDING COMPLETED!             ${NC}"
        echo -e "${GREEN}========================================${NC}"
        echo
        echo -e "${GREEN}Frontend test accounts created (password: 'password'):${NC}"
        echo -e "• ${YELLOW}admin@dasyin.com.au${NC} - Super Admin"
        echo -e "• ${YELLOW}kennedy@dasyin.com.au${NC} - Org Admin"
        echo -e "• ${YELLOW}manager@dasyin.com.au${NC} - Manager"
        echo -e "• ${YELLOW}coordinator@dasyin.com.au${NC} - Support Coordinator"
        echo -e "• ${YELLOW}careworker@dasyin.com.au${NC} - Care Worker"
        echo -e "• ${YELLOW}org2admin@dasyin.com.au${NC} - Org 2 Admin"
        echo
        echo -e "${GREEN}Organizations:${NC}"
        echo -e "• Sunshine Care Services"
        echo -e "• Melbourne Support Network"
        echo
        echo -e "${GREEN}Each organization includes:${NC}"
        echo -e "• 2 Admins, 2 Managers, 2 Coordinators, 2 Care Workers"
        echo -e "• 15-20 Participants with emergency contacts"
        echo -e "• 200+ realistic shifts (past month + upcoming week)"
        echo -e "• Care plans for all participants"
        echo
        echo -e "${GREEN}To remove all test data:${NC}"
        echo -e "${YELLOW}./scripts/seed.sh clean${NC}"
        echo
    else
        echo
        echo -e "${RED}========================================${NC}"
        echo -e "${RED}         SEEDING FAILED!                ${NC}"
        echo -e "${RED}========================================${NC}"
        echo -e "${RED}Check the error messages above.${NC}"
        exit 1
    fi
fi